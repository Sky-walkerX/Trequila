package crypto

import "testing"

// base64 of a 32-byte AES-256 key
const testKeyB64 = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="

// Round-trip + wrong-key are the two behaviors worth proving for crypto we can't eyeball.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := NewCipherFromBase64(testKeyB64)
	if err != nil {
		t.Fatalf("NewCipherFromBase64: %v", err)
	}
	const plain = "sk-test-secret-api-key-12345"
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if enc == plain {
		t.Fatal("ciphertext must not equal plaintext")
	}
	got, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plain {
		t.Fatalf("round-trip mismatch: got %q want %q", got, plain)
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	src, _ := NewCipherFromBase64(testKeyB64)
	enc, _ := src.Encrypt("secret")
	other, _ := NewCipher([]byte("FEDCBA9876543210FEDCBA9876543210"))
	if _, err := other.Decrypt(enc); err == nil {
		t.Fatal("expected error decrypting with a different key")
	}
}

func TestNewCipherRejectsBadKeyLength(t *testing.T) {
	if _, err := NewCipher([]byte("tooshort")); err == nil {
		t.Fatal("expected error for non-32-byte key")
	}
}
