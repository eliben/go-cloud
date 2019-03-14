// Copyright 2019 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package azurekeyvault

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/services/keyvault/v7.0/keyvault"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
	"gocloud.dev/internal/testing/setup"
	"gocloud.dev/secrets"
	"gocloud.dev/secrets/driver"
	"gocloud.dev/secrets/drivertest"
)

// Prerequisites for --record mode
//
// 1. Sign-in to your Azure Subscription at http://portal.azure.com.
//
// 2. Create a KeyVault, see https://docs.microsoft.com/en-us/azure/key-vault/quick-create-portal.
//
// 3. Choose an authentication model. This test uses Service Principal, see https://docs.microsoft.com/en-us/rest/api/azure/index#register-your-client-application-with-azure-ad.
// For documentation on acceptable auth models, see https://docs.microsoft.com/en-us/azure/key-vault/key-vault-whatis.
//
// 4. Set your environment variables depending on the auth model selection. Modify helper initEnv() as needed.
// For Service Principal, please set the following, see https://docs.microsoft.com/en-us/go/azure/azure-sdk-go-authorization.
// - AZURE_TENANT_ID (The ID for the Active Directory tenant that the service principal belongs to.)
// - AZURE_CLIENT_ID (The name or ID of the service principal.)
// - AZURE_CLIENT_SECRET (The secret associated with the service principal.)
// - AZURE_ENVIRONMENT
// - AZURE_AD_RESOURCE to https://vault.azure.net
//
// 5. Create/Import a Key. This can be done in the Azure Portal or by code.
//
// 6. Update constants below to match your Azure KeyVault settings.

const (
	keyVaultName = "go-cdk"
	keyID1       = "test1"
	keyID2       = "test2"
	// Important: an empty key version will default to 'Current Version' in Azure Key Vault.
	// See link below for more information on versioning
	// https://docs.microsoft.com/en-us/azure/key-vault/about-keys-secrets-and-certificates
	keyVersion = ""
	algorithm  = string(keyvault.RSAOAEP256)
)

type harness struct {
	client *keyvault.BaseClient
	close  func()
}

func (h *harness) MakeDriver(ctx context.Context) (driver.Keeper, driver.Keeper, error) {
	keeper1 := keeper{
		client:       h.client,
		keyVaultName: keyVaultName,
		keyName:      keyID1,
		keyVersion:   keyVersion,
		options:      &KeeperOptions{Algorithm: algorithm},
	}

	keeper2 := keeper{
		client:       h.client,
		keyVaultName: keyVaultName,
		keyName:      keyID2,
		keyVersion:   keyVersion,
		options:      &KeeperOptions{Algorithm: algorithm},
	}

	return &keeper1, &keeper2, nil
}

func (h *harness) Close() {
	h.close()
}

func newHarness(ctx context.Context, t *testing.T) (drivertest.Harness, error) {
	// Use initEnv to setup your environment variables.
	if *setup.Record {
		initEnv()
	}

	done, sender := setup.NewAzureKeyVaultTestClient(ctx, t)
	client, err := Dial()
	if err != nil {
		return nil, err
	}
	client.Sender = sender

	// Use a null authorizer for replay mode.
	if !*setup.Record {
		na := &autorest.NullAuthorizer{}
		client.Authorizer = na
	}

	return &harness{
		client: client,
		close:  done,
	}, nil
}

func initEnv() {
	env, err := azure.EnvironmentFromName("AZUREPUBLICCLOUD")
	if err != nil {
		log.Fatalln(err)
	}

	// For Client Credentials authorization, set AZURE_TENANT_ID, AZURE_CLIENT_ID, AZURE_CLIENT_SECRET
	// For Client Certificate and Azure Managed Service Identity, see doc below for help
	// https://github.com/Azure/azure-sdk-for-go

	if os.Getenv("AZURE_TENANT_ID") == "" ||
		os.Getenv("AZURE_CLIENT_ID") == "" ||
		os.Getenv("AZURE_CLIENT_SECRET") == "" {
		log.Fatal("Missing environment for recording tests, set AZURE_TENANT_ID, AZURE_CLIENT_ID and AZURE_CLIENT_SECRET")
	}

	os.Setenv("AZURE_ENVIRONMENT", env.Name)

	vaultEndpoint := strings.TrimSuffix(env.KeyVaultEndpoint, "/")
	os.Setenv("AZURE_AD_RESOURCE", vaultEndpoint)
}

func TestConformance(t *testing.T) {
	drivertest.RunConformanceTests(t, newHarness, []drivertest.AsTest{verifyAs{}})
}

type verifyAs struct{}

func (v verifyAs) Name() string {
	return "verify As function"
}

func (v verifyAs) ErrorCheck(k *secrets.Keeper, err error) error {
	var e autorest.DetailedError
	if !k.ErrorAs(err, &e) {
		return errors.New("Keeper.ErrorAs failed")
	}
	return nil
}

// Key Vault-specific tests.

func TestOpenKeeper(t *testing.T) {
	tests := []struct {
		URL     string
		WantErr bool
	}{
		// Missing algorithm query param.
		{"azurekeyvault://mykeyvault/mykey/myversion", true},
		// Invalid query parameter.
		{"azurekeyvault://mykeyvault/mykey/myversion?algorithm=RSA-OAEP-256&param=value", true},
		// Empty host.
		{"azurekeyvault:///mykey/myversion?algorithm=RSA-OAEP-256", true},
		// Path has 1 elements (no version) -> OK.
		{"azurekeyvault://mykeyvault/mykey?algorithm=RSA-OAEP-256", false},
		// Path has > 2 elements.
		{"azurekeyvault://mykeyvault/mykey/myversion/extra?algorithm=RSA-OAEP-256", true},
		// Path has empty first element.
		{"azurekeyvault://mykeyvault//myversion?algorithm=RSA-OAEP-256", true},
		// OK.
		{"azurekeyvault://mykeyvault/mykey/myversion?algorithm=RSA-OAEP-256", false},
	}

	ctx := context.Background()
	for _, test := range tests {
		_, err := secrets.OpenKeeper(ctx, test.URL)
		if (err != nil) != test.WantErr {
			t.Errorf("%s: got error %v, want error %v", test.URL, err, test.WantErr)
		}
	}
}

func TestNoConnectionError(t *testing.T) {
	client := keyvault.NewWithoutDefaults()
	k, err := NewKeeper(&client, keyVaultName, keyID1, keyVersion, &KeeperOptions{Algorithm: algorithm})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Encrypt(context.Background(), []byte("secrets")); err == nil {
		t.Error("Encrypt: got nil, want no connection error")
	}
}

func TestAlgorithmNotProvided(t *testing.T) {
	client := keyvault.NewWithoutDefaults()
	if _, err := NewKeeper(&client, keyVaultName, keyID1, keyVersion, nil); err == nil {
		t.Error("NewKeeper with no algorithm: got nil, want no algorithm error")
	}
}
