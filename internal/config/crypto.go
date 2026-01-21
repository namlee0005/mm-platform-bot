package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"golang.org/x/crypto/pbkdf2"
)

// EncryptedUserSecret represents the encrypted user secret from MongoDB
type EncryptedUserSecret struct {
	EncryptedSecret string `json:"encryptedSecret" bson:"encryptedSecret"`
	IV              string `json:"iv" bson:"iv"`
	Tag             string `json:"tag" bson:"tag"`
}

// UnmarshalBSONValue handles both JSON string and embedded document formats
func (e *EncryptedUserSecret) UnmarshalBSONValue(t bsontype.Type, data []byte) error {
	if t == bsontype.String {
		// It's a JSON string, parse it
		var raw bson.RawValue
		raw.Type = t
		raw.Value = data
		var jsonStr string
		if err := raw.Unmarshal(&jsonStr); err != nil {
			return err
		}
		return json.Unmarshal([]byte(jsonStr), e)
	}
	// It's an embedded document
	var doc struct {
		EncryptedSecret string `bson:"encryptedSecret"`
		IV              string `bson:"iv"`
		Tag             string `bson:"tag"`
	}
	if err := bson.Unmarshal(data, &doc); err != nil {
		return err
	}
	e.EncryptedSecret = doc.EncryptedSecret
	e.IV = doc.IV
	e.Tag = doc.Tag
	return nil
}

// EncryptedData represents encrypted API key/secret from MongoDB
type EncryptedData struct {
	Encrypted string `json:"encrypted" bson:"encrypted"`
	IV        string `json:"iv" bson:"iv"`
	Tag       string `json:"tag" bson:"tag"`
	Salt      string `json:"salt" bson:"salt"`
}

// UnmarshalBSONValue handles both JSON string and embedded document formats
func (e *EncryptedData) UnmarshalBSONValue(t bsontype.Type, data []byte) error {
	// Handle null values
	if t == bsontype.Null {
		return nil
	}

	if t == bsontype.String {
		// It's a JSON string, parse it
		var raw bson.RawValue
		raw.Type = t
		raw.Value = data
		var jsonStr string
		if err := raw.Unmarshal(&jsonStr); err != nil {
			return err
		}
		// Handle empty string
		if jsonStr == "" {
			return nil
		}
		return json.Unmarshal([]byte(jsonStr), e)
	}
	// It's an embedded document
	var doc struct {
		Encrypted string `bson:"encrypted"`
		IV        string `bson:"iv"`
		Tag       string `bson:"tag"`
		Salt      string `bson:"salt"`
	}
	if err := bson.Unmarshal(data, &doc); err != nil {
		return err
	}
	e.Encrypted = doc.Encrypted
	e.IV = doc.IV
	e.Tag = doc.Tag
	e.Salt = doc.Salt
	return nil
}

// deriveKey derives a key from secret using PBKDF2 (matches TypeScript implementation)
// Note: TypeScript passes salt as string directly to pbkdf2, not as decoded bytes
func deriveKey(secret, salt string) []byte {
	// PBKDF2 with 100000 iterations, 32 bytes key, SHA-512
	// Salt is used as UTF-8 string (same as TypeScript)
	key := pbkdf2.Key([]byte(secret), []byte(salt), 100000, 32, sha512.New)
	return key
}

// decryptAESGCM decrypts data using AES-256-GCM
func decryptAESGCM(key, ciphertext, iv, tag []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// In Go's GCM, the tag is appended to the ciphertext
	ciphertextWithTag := append(ciphertext, tag...)

	plaintext, err := aesGCM.Open(nil, iv, ciphertextWithTag, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}

	return plaintext, nil
}

// DecryptUserSecret decrypts the user secret using APP_MASTER_KEY
func DecryptUserSecret(encryptedData EncryptedUserSecret, masterKeyHex string) (string, error) {
	if masterKeyHex == "" {
		return "", fmt.Errorf("APP_MASTER_KEY is not defined")
	}

	masterKey, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return "", fmt.Errorf("failed to decode master key: %w", err)
	}

	encryptedSecret, err := hex.DecodeString(encryptedData.EncryptedSecret)
	if err != nil {
		return "", fmt.Errorf("failed to decode encrypted secret: %w", err)
	}

	iv, err := hex.DecodeString(encryptedData.IV)
	if err != nil {
		return "", fmt.Errorf("failed to decode IV: %w", err)
	}

	tag, err := hex.DecodeString(encryptedData.Tag)
	if err != nil {
		return "", fmt.Errorf("failed to decode tag: %w", err)
	}

	plaintext, err := decryptAESGCM(masterKey, encryptedSecret, iv, tag)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt user secret: %w", err)
	}

	return string(plaintext), nil
}

// DecryptPrivateKey decrypts API key/secret using the user secret
func DecryptPrivateKey(encryptedData EncryptedData, userSecret string) (string, error) {
	key := deriveKey(userSecret, encryptedData.Salt)

	encrypted, err := hex.DecodeString(encryptedData.Encrypted)
	if err != nil {
		return "", fmt.Errorf("failed to decode encrypted data: %w", err)
	}

	iv, err := hex.DecodeString(encryptedData.IV)
	if err != nil {
		return "", fmt.Errorf("failed to decode IV: %w", err)
	}

	tag, err := hex.DecodeString(encryptedData.Tag)
	if err != nil {
		return "", fmt.Errorf("failed to decode tag: %w", err)
	}

	plaintext, err := decryptAESGCM(key, encrypted, iv, tag)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt private key: %w", err)
	}

	return string(plaintext), nil
}
