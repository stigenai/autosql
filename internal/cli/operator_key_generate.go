package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
)

func runOperatorKeyGenerate(args []string, o output) error {
	fs := newFlags("operator key generate", o.streams.Err)
	privatePath := fs.String("private-output", "", "new private-key file")
	publicPath := fs.String("public-output", "", "new public-key file")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *privatePath == "" || *publicPath == "" || *privatePath == *publicPath {
		return usageError(errors.New("distinct --private-output and --public-output paths are required"))
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return &Error{Kind: "validation", Message: "generate Ed25519 key failed", Code: ExitValidation}
	}
	if err = atomicCreate(*privatePath, []byte(base64.RawStdEncoding.EncodeToString(private)+"\n")); err != nil {
		return &Error{Kind: "conflict", Message: "create private key failed", Code: ExitConflict}
	}
	if err = atomicCreate(*publicPath, []byte(base64.RawStdEncoding.EncodeToString(public)+"\n")); err != nil {
		_ = os.Remove(*privatePath)
		return &Error{Kind: "conflict", Message: "create public key failed", Code: ExitConflict}
	}
	o.json = *jsonFlag
	return o.success(map[string]string{"status": "generated", "private_output": *privatePath, "public_output": *publicPath}, "generated Ed25519 key pair")
}
