/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vault

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	vault "github.com/hashicorp/vault/api"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	esv1 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1"
	esmeta "github.com/external-secrets/external-secrets/apis/meta/v1"
	"github.com/external-secrets/external-secrets/pkg/constants"
	"github.com/external-secrets/external-secrets/pkg/metrics"
	vaultiamauth "github.com/external-secrets/external-secrets/pkg/provider/vault/iamauth"
	"github.com/external-secrets/external-secrets/pkg/provider/vault/util"
)

const (
	errAuthFormat            = "cannot initialize Vault client: no valid auth method specified"
	errVaultToken            = "cannot parse Vault authentication token: %w"
	errGetKubeSATokenRequest = "cannot request Kubernetes service account token for service account %q: %w"
	errVaultRevokeToken      = "error while revoking token: %w"
)

// setAuth gets a new token using the configured mechanism.
// If there's already a valid token, does nothing.
func (c *client) setAuth(ctx context.Context, cfg *vault.Config) error {
	if c.store.Auth == nil {
		return nil
	}

	if c.store.Namespace != nil { // set namespace before checking the need for AuthNamespace
		c.client.SetNamespace(*c.store.Namespace)
	}

	// Switch to auth namespace if different from the provider namespace
	restoreNamespace := c.useAuthNamespace(ctx)
	defer restoreNamespace()

	tokenExists := false
	var err error
	if c.client.Token() != "" {
		tokenExists, err = checkToken(ctx, c.token)
	}
	if tokenExists {
		c.log.V(1).Info("Re-using existing token")
		return err
	}

	tokenExists, err = setSecretKeyToken(ctx, c)
	if tokenExists {
		c.log.V(1).Info("Set token from secret")
		return err
	}

	tokenExists, err = setAppRoleToken(ctx, c)
	if tokenExists {
		c.log.V(1).Info("Retrieved new token using AppRole auth")
		return err
	}

	tokenExists, err = setKubernetesAuthToken(ctx, c)
	if tokenExists {
		c.log.V(1).Info("Retrieved new token using Kubernetes auth")
		return err
	}

	tokenExists, err = setLdapAuthToken(ctx, c)
	if tokenExists {
		c.log.V(1).Info("Retrieved new token using LDAP auth")
		return err
	}

	tokenExists, err = setUserPassAuthToken(ctx, c)
	if tokenExists {
		c.log.V(1).Info("Retrieved new token using userPass auth")
		return err
	}
	tokenExists, err = setJwtAuthToken(ctx, c)
	if tokenExists {
		c.log.V(1).Info("Retrieved new token using JWT auth")
		return err
	}

	tokenExists, err = setCertAuthToken(ctx, c, cfg)
	if tokenExists {
		c.log.V(1).Info("Retrieved new token using certificate auth")
		return err
	}

	tokenExists, err = setIamAuthToken(ctx, c, vaultiamauth.DefaultJWTProvider, vaultiamauth.DefaultSTSProvider)
	if tokenExists {
		c.log.V(1).Info("Retrieved new token using IAM auth")
		return err
	}

	return errors.New(errAuthFormat)
}

func createServiceAccountToken(
	ctx context.Context,
	corev1Client typedcorev1.CoreV1Interface,
	storeKind string,
	namespace string,
	serviceAccountRef esmeta.ServiceAccountSelector,
	additionalAud []string,
	expirationSeconds int64) (string, error) {
	audiences := serviceAccountRef.Audiences
	if len(additionalAud) > 0 {
		audiences = append(audiences, additionalAud...)
	}
	tokenRequest := &authv1.TokenRequest{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
		},
		Spec: authv1.TokenRequestSpec{
			Audiences:         audiences,
			ExpirationSeconds: &expirationSeconds,
		},
	}
	if (storeKind == esv1.ClusterSecretStoreKind) &&
		(serviceAccountRef.Namespace != nil) {
		tokenRequest.Namespace = *serviceAccountRef.Namespace
	}
	tokenResponse, err := corev1Client.ServiceAccounts(tokenRequest.Namespace).
		CreateToken(ctx, serviceAccountRef.Name, tokenRequest, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf(errGetKubeSATokenRequest, serviceAccountRef.Name, err)
	}
	return tokenResponse.Status.Token, nil
}

// checkToken does a lookup and checks if the provided token exists.
func checkToken(ctx context.Context, token util.Token) (bool, error) {
	// https://www.vaultproject.io/api-docs/auth/token#lookup-a-token-self
	resp, err := token.LookupSelfWithContext(ctx)
	metrics.ObserveAPICall(constants.ProviderHCVault, constants.CallHCVaultLookupSelf, err)
	if err != nil {
		return false, err
	}
	// LookupSelfWithContext() calls ParseSecret(), which has several places
	// that return no data and no error, including when a token is expired.
	if resp == nil {
		return false, errors.New("no response nor error for token lookup")
	}
	t, ok := resp.Data["type"]
	if !ok {
		return false, errors.New("could not assert token type")
	}
	tokenType := t.(string)
	if tokenType == "batch" {
		return false, nil
	}
	ttl, ok := resp.Data["ttl"]
	if !ok {
		return false, errors.New("no TTL found in response")
	}
	ttlInt, err := ttl.(json.Number).Int64()
	if err != nil {
		return false, fmt.Errorf("invalid token TTL: %v: %w", ttl, err)
	}
	expireTime, ok := resp.Data["expire_time"]
	if !ok {
		return false, errors.New("no expiration time found in response")
	}
	if ttlInt < 60 && expireTime != nil {
		// Treat expirable tokens that are about to expire as already expired.
		// This ensures that the token won't expire in between this check and
		// performing the actual operation.
		return false, nil
	}
	return true, nil
}

func revokeTokenIfValid(ctx context.Context, client util.Client) error {
	valid, err := checkToken(ctx, client.AuthToken())
	if err != nil {
		return fmt.Errorf(errVaultRevokeToken, err)
	}
	if valid {
		err = client.AuthToken().RevokeSelfWithContext(ctx, client.Token())
		metrics.ObserveAPICall(constants.ProviderHCVault, constants.CallHCVaultRevokeSelf, err)
		if err != nil {
			return fmt.Errorf(errVaultRevokeToken, err)
		}
		client.ClearToken()
	}
	return nil
}

func (c *client) useAuthNamespace(_ context.Context) func() {
	ns := ""
	if c.store != nil && c.store.Namespace != nil {
		ns = *c.store.Namespace
	}

	if c.store.Auth != nil && c.store.Auth.Namespace != nil {
		// Different Auth Vault Namespace than Secret Vault Namespace
		// Switch namespaces then switch back at the end
		if c.store.Auth.Namespace != nil && *c.store.Auth.Namespace != ns {
			c.log.V(1).Info("Using namespace=%s for the vault login", *c.store.Auth.Namespace)
			c.client.SetNamespace(*c.store.Auth.Namespace)
			// use this as a defer to reset the namespace
			return func() {
				c.log.V(1).Info("Restoring client namespace to namespace=%s", ns)
				c.client.SetNamespace(ns)
			}
		}
	}

	return func() {
		// no-op
	}
}
