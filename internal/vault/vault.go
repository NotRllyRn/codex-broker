package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/NotRllyRn/codex-broker/internal/core"
	"golang.org/x/crypto/hkdf"
)

const Prefix = "wk1_"

type Envelope struct {
	BundleID             string
	AccountID            string
	KeyID                string
	Nonce                []byte
	Ciphertext           []byte
	AAD                  []byte
	PayloadSchemaVersion int
	EnvelopeVersion      int
}

type Vault struct {
	rootKey    []byte
	InstanceID string
	KeyID      string
}

type credentialFile struct {
	RelativePath  string `json:"relative_path"`
	Mode          int    `json:"mode"`
	SHA256        string `json:"sha256"`
	ContentBase64 string `json:"content_base64"`
}

type Payload struct {
	SchemaVersion       int              `json:"schema_version"`
	CodexVersion        string           `json:"codex_version,omitempty"`
	Files               []credentialFile `json:"files,omitempty"`
	WorkspaceConstraint *string          `json:"workspace_constraint"`
	Value               *string          `json:"value,omitempty"`
}

func GenerateKey() string { return Prefix + core.RandomToken(32) }

func DecodeKey(encoded string) ([]byte, error) {
	if len(encoded) < len(Prefix) || encoded[:len(Prefix)] != Prefix {
		return nil, errors.New("vault key must use wk1_ format")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded[len(Prefix):])
	if err != nil {
		return nil, errors.New("vault key is not valid base64url")
	}
	if len(raw) != 32 {
		return nil, errors.New("vault key must contain exactly 32 random bytes")
	}
	return raw, nil
}

func New(root []byte, instanceID string, keyID ...string) (*Vault, error) {
	if len(root) != 32 {
		return nil, errors.New("root key must be 32 bytes")
	}
	id := "primary"
	if len(keyID) > 0 {
		id = keyID[0]
	}
	return &Vault{rootKey: append([]byte(nil), root...), InstanceID: instanceID, KeyID: id}, nil
}

func (v *Vault) accountKey(accountID, keyID string) ([]byte, error) {
	reader := hkdf.New(sha256.New, v.rootKey, []byte(v.InstanceID), []byte("windowkeeper/credential-bundle/v1:"+keyID+":"+accountID))
	key := make([]byte, 32)
	_, err := io.ReadFull(reader, key)
	return key, err
}

func (v *Vault) Encrypt(accountID string, payload Payload) (Envelope, error) {
	bundleID := core.NewID()
	aad, err := json.Marshal(struct {
		AccountID            string `json:"account_id"`
		BundleID             string `json:"bundle_id"`
		EnvelopeVersion      int    `json:"envelope_version"`
		InstanceID           string `json:"instance_id"`
		KeyID                string `json:"key_id"`
		PayloadSchemaVersion int    `json:"payload_schema_version"`
	}{accountID, bundleID, 1, v.InstanceID, v.KeyID, 1})
	if err != nil {
		return Envelope{}, err
	}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	key, err := v.accountKey(accountID, v.KeyID)
	if err != nil {
		return Envelope{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Envelope{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, err
	}
	return Envelope{bundleID, accountID, v.KeyID, nonce, gcm.Seal(nil, nonce, plaintext, aad), aad, 1, 1}, nil
}

func (v *Vault) Decrypt(envelope Envelope) (Payload, error) {
	key, err := v.accountKey(envelope.AccountID, envelope.KeyID)
	if err != nil {
		return Payload{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Payload{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Payload{}, err
	}
	plaintext, err := gcm.Open(nil, envelope.Nonce, envelope.Ciphertext, envelope.AAD)
	if err != nil {
		return Payload{}, err
	}
	var payload Payload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return Payload{}, errors.New("credential payload is not valid JSON")
	}
	if payload.SchemaVersion != 1 {
		return Payload{}, errors.New("unsupported credential payload")
	}
	return payload, nil
}

func (v *Vault) Capture(codexHome, codexVersion string, workspace *string) (Payload, error) {
	path := filepath.Join(codexHome, "auth.json")
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
		return Payload{}, errors.New("unsafe or missing credential file: auth.json")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return Payload{}, err
	}
	var object map[string]any
	if json.Unmarshal(content, &object) != nil || object == nil {
		return Payload{}, errors.New("auth.json is not valid JSON")
	}
	digest := sha256.Sum256(content)
	return Payload{
		SchemaVersion: 1,
		CodexVersion:  codexVersion,
		Files: []credentialFile{{
			RelativePath: "auth.json", Mode: 0o600,
			SHA256: hex.EncodeToString(digest[:]), ContentBase64: base64.StdEncoding.EncodeToString(content),
		}},
		WorkspaceConstraint: workspace,
	}, nil
}

func (v *Vault) AuthJSON(payload Payload) ([]byte, error) {
	for _, file := range payload.Files {
		if file.RelativePath != "auth.json" {
			continue
		}
		content, err := base64.StdEncoding.Strict().DecodeString(file.ContentBase64)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(content)
		if hex.EncodeToString(digest[:]) != file.SHA256 {
			return nil, errors.New("credential payload digest mismatch")
		}
		var object map[string]any
		if json.Unmarshal(content, &object) != nil || object == nil {
			return nil, errors.New("auth.json is not valid JSON")
		}
		return content, nil
	}
	return nil, errors.New("credential bundle has no auth.json")
}

func (v *Vault) AuthFingerprint(payload Payload) (string, error) {
	if _, err := v.AuthJSON(payload); err != nil {
		return "", err
	}
	for _, file := range payload.Files {
		if file.RelativePath == "auth.json" {
			return file.SHA256, nil
		}
	}
	return "", errors.New("credential bundle has no auth.json")
}

func (v *Vault) Materialize(payload Payload, destination string) error {
	if _, err := v.AuthJSON(payload); err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return err
	}
	for _, file := range payload.Files {
		if file.RelativePath != "auth.json" && file.RelativePath != "config.toml" || filepath.IsAbs(file.RelativePath) || filepath.Clean(file.RelativePath) != file.RelativePath {
			return errors.New("credential payload contains an unsafe path")
		}
		if file.RelativePath == "config.toml" {
			continue
		}
		content, err := base64.StdEncoding.Strict().DecodeString(file.ContentBase64)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		if hex.EncodeToString(digest[:]) != file.SHA256 {
			return errors.New("credential payload digest mismatch")
		}
		path := filepath.Join(destination, "auth.json")
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		written := 0
		for written < len(content) {
			count, writeErr := syscall.Write(fd, content[written:])
			if writeErr != nil || count <= 0 {
				syscall.Close(fd)
				if writeErr != nil {
					return writeErr
				}
				return errors.New("credential file write did not progress")
			}
			written += count
		}
		if err := syscall.Fsync(fd); err != nil {
			syscall.Close(fd)
			return err
		}
		if err := syscall.Close(fd); err != nil {
			return err
		}
	}
	return nil
}

func (v *Vault) SealText(scope, value string) ([]byte, error) {
	payload := Payload{SchemaVersion: 1, Value: &value}
	envelope, err := v.Encrypt(scope, payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		BundleID   string `json:"bundle_id"`
		AccountID  string `json:"account_id"`
		KeyID      string `json:"key_id"`
		Nonce      string `json:"nonce"`
		Ciphertext string `json:"ciphertext"`
		AAD        string `json:"aad"`
	}{
		envelope.BundleID, envelope.AccountID, envelope.KeyID,
		base64.StdEncoding.EncodeToString(envelope.Nonce),
		base64.StdEncoding.EncodeToString(envelope.Ciphertext),
		base64.StdEncoding.EncodeToString(envelope.AAD),
	})
}

func (v *Vault) OpenText(value []byte) (string, error) {
	var stored struct {
		BundleID   string `json:"bundle_id"`
		AccountID  string `json:"account_id"`
		KeyID      string `json:"key_id"`
		Nonce      string `json:"nonce"`
		Ciphertext string `json:"ciphertext"`
		AAD        string `json:"aad"`
	}
	if err := json.Unmarshal(value, &stored); err != nil {
		return "", errors.New("sealed text is invalid")
	}
	decode := func(value string) ([]byte, error) { return base64.StdEncoding.Strict().DecodeString(value) }
	nonce, err := decode(stored.Nonce)
	if err != nil {
		return "", errors.New("sealed text is invalid")
	}
	ciphertext, err := decode(stored.Ciphertext)
	if err != nil {
		return "", errors.New("sealed text is invalid")
	}
	aad, err := decode(stored.AAD)
	if err != nil {
		return "", errors.New("sealed text is invalid")
	}
	payload, err := v.Decrypt(Envelope{stored.BundleID, stored.AccountID, stored.KeyID, nonce, ciphertext, aad, 1, 1})
	if err != nil || payload.Value == nil {
		return "", errors.New("sealed text is invalid")
	}
	return *payload.Value, nil
}
