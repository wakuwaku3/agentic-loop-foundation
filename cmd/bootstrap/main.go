package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/takushi/agentic-loop-foundation/v2/internal/update"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("0.1.0-dev")
		return
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bootstrap:", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bootstrap install|switch|--version")
	}
	switch args[0] {
	case "install":
		flags := flag.NewFlagSet("bootstrap install", flag.ContinueOnError)
		root := flags.String("root", "", "absolute bootstrap root")
		manifestPath := flags.String("manifest", "", "signed manifest path")
		binaryPath := flags.String("binary", "", "runner binary path")
		signaturePath := flags.String("signature", "", "base64 signature path")
		publicKeyPath := flags.String("public-key", "", "base64 Ed25519 public key path")
		schema := flags.Int("schema", 0, "current canonical schema version")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *schema <= 0 {
			return errors.New("invalid install arguments")
		}
		manifest, err := os.ReadFile(*manifestPath)
		if err != nil {
			return err
		}
		binary, err := os.ReadFile(*binaryPath)
		if err != nil {
			return err
		}
		signature, err := readBase64(*signaturePath, ed25519.SignatureSize)
		if err != nil {
			return err
		}
		publicKey, err := readBase64(*publicKeyPath, ed25519.PublicKeySize)
		if err != nil {
			return err
		}
		installed, err := update.Install(*root, update.Bundle{Manifest: manifest, Binary: binary, Signature: signature}, ed25519.PublicKey(publicKey), *schema)
		if err != nil {
			return err
		}
		fmt.Println(installed.Version)
		return nil
	case "switch":
		flags := flag.NewFlagSet("bootstrap switch", flag.ContinueOnError)
		root := flags.String("root", "", "absolute bootstrap root")
		channel := flags.String("channel", "", "stable or preview")
		version := flags.String("version", "", "installed semantic version")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("invalid switch arguments")
		}
		return update.Switch(*root, *channel, *version)
	default:
		return errors.New("usage: bootstrap install|switch|--version")
	}
}

func readBase64(path string, size int) ([]byte, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(value)))
	if err != nil || len(decoded) != size {
		return nil, errors.New("invalid base64 signing material")
	}
	return decoded, nil
}
