// Copyright 2018 The Go Cloud Development Kit Authors
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

package localsecrets_test

import (
	"context"
	"fmt"
	"log"

	"gocloud.dev/secrets"
	"gocloud.dev/secrets/localsecrets"
)

func Example() {
	// localsecrets.Keeper untilizes the golang.org/x/crypto/nacl/secretbox package
	// for the crypto implementation, and secretbox requires a secret key
	// that is a [32]byte. Because most users will have keys which are strings,
	// the localsecrets package supplies a helper function to convert your key
	// and also crop it to size, if necessary.
	secretKey := localsecrets.ByteKey("I'm a secret string!")
	keeper := localsecrets.NewKeeper(secretKey)

	// Now we can use keeper to encrypt or decrypt.
	plaintext := []byte("Hello, Secrets!")
	ctx := context.Background()
	ciphertext, err := keeper.Encrypt(ctx, plaintext)
	if err != nil {
		log.Fatal(err)
	}
	decrypted, err := keeper.Decrypt(ctx, ciphertext)
	fmt.Println(string(decrypted))

	// Output:
	// Hello, Secrets!
}

func Example_openKeeper() {
	ctx := context.Background()

	// OpenKeeper creates a *secrets.Keeper from a URL.
	// Using "stringkey://", the first 32 bytes of the URL hostname is used as the secret.
	k, err := secrets.OpenKeeper(ctx, "stringkey://my-secret-key")

	// Using "base64key://", the URL hostname must be a base64-encoded key.
	// The first 32 bytes of the decoding are used as the secret key.
	k, err = secrets.OpenKeeper(ctx, "base64key://bXktc2VjcmV0LWtleQ==")
	_, _ = k, err
}
