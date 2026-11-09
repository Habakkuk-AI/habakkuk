package wecom

import "testing"

// The URL verification example from WeCom's callback documentation.
func TestKnownVector(t *testing.T) {
	c, err := NewCrypt("QDG6eK", "jWmYm7qr5nMoAUwZRjGtBxmz3KA1tkAj3ykkR6q2B2C", "wx5823bf96d3bd56c7")
	if err != nil {
		t.Fatal(err)
	}
	echo := "P9nAzCzyDtyTWESHep1vC5X9xho/qYX3Zpb4yKa9SKld1DsH3Iyt3tP3zNdtp+4RPcs8TgAE7OaBO+FZXvnaqQ=="
	if !c.Verify("5c45ff5e21c57e6ad56bac8758b79b1d9ac89fd3", "1409659589", "263014780", echo) {
		t.Fatalf("signature = %s", c.Signature("1409659589", "263014780", echo))
	}
	got, err := c.Decrypt(echo)
	if err != nil || string(got) != "1616140317555161061" {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
}

func TestRoundTrip(t *testing.T) {
	c, _ := NewCrypt("tok", "jWmYm7qr5nMoAUwZRjGtBxmz3KA1tkAj3ykkR6q2B2C", "corp1")
	for _, s := range []string{"", "x", "<xml><Content><![CDATA[你好]]></Content></xml>", string(make([]byte, 12))} {
		got, err := c.Decrypt(c.Encrypt([]byte(s)))
		if err != nil || string(got) != s {
			t.Fatalf("round trip %q = %q, %v", s, got, err)
		}
	}
	other, _ := NewCrypt("tok", "jWmYm7qr5nMoAUwZRjGtBxmz3KA1tkAj3ykkR6q2B2C", "corp2")
	if _, err := other.Decrypt(c.Encrypt([]byte("hi"))); err == nil {
		t.Fatal("a message for another corp decrypted")
	}
	if _, err := NewCrypt("t", "short", ""); err == nil {
		t.Fatal("short key accepted")
	}
}
