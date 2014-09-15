/*
 * Copyright (c) 2014 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/conformal/btcutil/hdkeychain"
	"github.com/conformal/btcwallet/legacy/keystore"
	"github.com/conformal/btcwallet/waddrmgr"
)

// promptConsoleList prompts the user with the given prefix, list of valid
// responses, and default list entry to use.  The function will repeat the
// prompt to the user until they enter a valid response.
func promptConsoleList(reader *bufio.Reader, prefix string, validResponses []string, defaultEntry string) (string, error) {
	// Setup the prompt according to the parameters.
	validStrings := strings.Join(validResponses, "/")
	var prompt string
	if defaultEntry != "" {
		prompt = fmt.Sprintf("%s (%s) [%s]: ", prefix, validStrings,
			defaultEntry)
	} else {
		prompt = fmt.Sprintf("%s (%s): ", prefix, validStrings)
	}

	// Prompt the user until one of the valid responses is given.
	for {
		fmt.Print(prompt)
		reply, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		reply = strings.TrimSpace(strings.ToLower(reply))
		if reply == "" {
			reply = defaultEntry
		}

		for _, validResponse := range validResponses {
			if reply == validResponse {
				return reply, nil
			}
		}
	}
}

// promptConsoleListBool prompts the user for a boolean (yes/no) with the given
// prefix.  The function will repeat the prompt to the user until they enter a
// valid reponse.
func promptConsoleListBool(reader *bufio.Reader, prefix string, defaultEntry string) (bool, error) {
	// Setup the valid responses.
	valid := []string{"n", "no", "y", "yes"}
	response, err := promptConsoleList(reader, prefix, valid, defaultEntry)
	if err != nil {
		return false, err
	}
	return response == "yes" || response == "y", nil
}

// promptConsolePass prompts the user for a passphrase with the given prefix.
// The function will ask the user to confirm the passphrase and will repeat
// the prompts until they enter a matching response.
func promptConsolePass(reader *bufio.Reader, prefix string) (string, error) {
	// Prompt the user until they enter a passphrase.
	prompt := fmt.Sprintf("%s: ", prefix)
	for {
		fmt.Print(prompt)
		pass, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		pass = strings.TrimSpace(pass)
		if pass == "" {
			continue
		}

		fmt.Print("Confirm passphrase: ")
		confirm, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		confirm = strings.TrimSpace(confirm)
		if pass != confirm {
			fmt.Println("The entered passphrases do not match")
			continue
		}

		return pass, nil
	}
}

// promptConsolePublicPass prompts the user whether they want to add an
// additional layer of encryption to the wallet.  When the user answers yes and
// there is already a public passphrase provided via the passed config,  it
// prompts them whether or not to use that configured passphrase.  It will also
// detect when the same passphrase is used for the private and public passphrase
// and prompt the user if they are sure they want to use the same passphrase for
// both.  Finally, all prompts are repeated until the user enters a valid
// response.
func promptConsolePublicPass(reader *bufio.Reader, privPass string, cfg *config) (string, error) {
	pubPass := walletPubPassphrase
	usePubPass, err := promptConsoleListBool(reader, "Do you want "+
		"to add an additional layer of encryption for public "+
		"data?", "no")
	if err != nil {
		return "", err
	}

	if !usePubPass {
		return pubPass, nil
	}

	if cfg.WalletPass != pubPass {
		useExisting, err := promptConsoleListBool(reader, "Use the "+
			"existing configured public passphrase for encryption "+
			"of public data?", "no")
		if err != nil {
			return "", err
		}

		if useExisting {
			return cfg.WalletPass, nil
		}
	}

	for {
		pubPass, err = promptConsolePass(reader, "Enter the public "+
			"passphrase for your new wallet")
		if err != nil {
			return "", err
		}

		if pubPass == privPass {
			useSamePass, err := promptConsoleListBool(reader,
				"Are you sure want to use the same passphrase "+
					"for public and private data?", "no")
			if err != nil {
				return "", err
			}

			if useSamePass {
				break
			}

			continue
		}

		break
	}

	fmt.Println("NOTE: Use the --walletpass option to configure your " +
		"public passphrase.")
	return pubPass, nil
}

// promptConsoleSeed prompts the user whether they want to use an existing
// wallet generation seed.  When the user answers no, a seed will be generated
// and displayed to the user along with prompting them for confirmation.  When
// the user answers yes, a the user is prompted for it.  All prompts are
// repeated until the user enters a valid response.
func promptConsoleSeed(reader *bufio.Reader) ([]byte, error) {
	// Ascertain the wallet generation seed.
	useUserSeed, err := promptConsoleListBool(reader, "Do you have an "+
		"existing wallet seed you want to use?", "no")
	if err != nil {
		return nil, err
	}
	if !useUserSeed {
		seed, err := hdkeychain.GenerateSeed(hdkeychain.RecommendedSeedLen)
		if err != nil {
			return nil, err
		}

		fmt.Println("Your wallet generation seed is:")
		fmt.Printf("%x\n", seed)
		fmt.Println("IMPORTANT: Keep the seed in a safe place as you " +
			"will NOT be able to restore your wallet without it.")
		fmt.Println("Please keep in mind that anyone who has access " +
			"to the seed can also restore your wallet thereby " +
			"giving them access to all your funds, so it is " +
			"imperative that you keep it in a secure location.")

		for {
			fmt.Print(`Once you have stored the seed in a safe ` +
				`and secure location, enter "OK" to continue: `)
			confirmSeed, err := reader.ReadString('\n')
			if err != nil {
				return nil, err
			}
			confirmSeed = strings.TrimSpace(confirmSeed)
			confirmSeed = strings.Trim(confirmSeed, `"`)
			if confirmSeed == "OK" {
				break
			}
		}

		return seed, nil
	}

	for {
		fmt.Print("Enter existing wallet seed: ")
		seedStr, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		seedStr = strings.TrimSpace(strings.ToLower(seedStr))

		seed, err := hex.DecodeString(seedStr)
		if err != nil || len(seed) < hdkeychain.MinSeedBytes ||
			len(seed) > hdkeychain.MaxSeedBytes {

			fmt.Printf("Invalid seed specified.  Must be a "+
				"hexadecimal value that is at least %d bits and "+
				"at most %d bits\n", hdkeychain.MinSeedBytes*8,
				hdkeychain.MaxSeedBytes*8)
			continue
		}

		return seed, nil
	}
}

// createWallet prompts the user for information needed to generate a new wallet
// and generates the wallet accordingly.  The new wallet will reside at the
// provided path.
func createWallet(mgrPath string, cfg *config) error {
	// Start by prompting for the private passphrase.
	reader := bufio.NewReader(os.Stdin)
	privPass, err := promptConsolePass(reader, "Enter the private passphrase "+
		"for your new wallet")
	if err != nil {
		return err
	}

	// Ascertain the public passphrase.  This will either be a value
	// specified by the user or the default hard-coded public passphrase if
	// the user does not want the additional public data encryption.
	pubPass, err := promptConsolePublicPass(reader, privPass, cfg)
	if err != nil {
		return err
	}

	// Ascertain the wallet generation seed.  This will either be an
	// automatically generated value the user has already confirmed or a
	// value the user has entered which has already been validated.
	seed, err := promptConsoleSeed(reader)
	if err != nil {
		return err
	}

	// Create the wallet.
	fmt.Println("Creating the wallet...")
	wallet, err := waddrmgr.Create(mgrPath, seed, []byte(pubPass),
		[]byte(privPass), activeNet.Params)
	if err != nil {
		return err
	}
	wallet.Close()
	fmt.Println("The wallet has been created successfully.")
	return nil
}
