package feishu

import "testing"

// The example from Feishu's event subscription guide.
func TestDecryptKnownVector(t *testing.T) {
	got, err := Decrypt("test key", "P37w+VZImNgPEO1RBhJ6RtKl7n6zymIbEG1pReEzghk=")
	if err != nil || string(got) != "hello world" {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
}

func TestEncryptRoundTrip(t *testing.T) {
	for _, s := range []string{"", "x", "exactly sixteen!", `{"challenge":"ajls384kdjx98XX","token":"xxxxxx","type":"url_verification"}`} {
		got, err := Decrypt("k3y", Encrypt("k3y", []byte(s)))
		if err != nil || string(got) != s {
			t.Fatalf("round trip %q = %q, %v", s, got, err)
		}
	}
	if _, err := Decrypt("other", Encrypt("k3y", []byte("secret message here"))); err == nil {
		t.Log("wrong key decrypted without a padding error (possible, 1 in 256)")
	}
}
