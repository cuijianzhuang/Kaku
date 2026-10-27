package secure

import "testing"

func TestBox(t *testing.T) {
	key := NewMasterKey()
	raw, err := ParseMasterKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewBox(raw)
	enc := b.Seal("CARD-123")
	if enc == "" || enc == b.Seal("CARD-123") {
		t.Fatal("seal must be randomized")
	}
	if got, err := b.Open(enc); err != nil || got != "CARD-123" {
		t.Fatalf("open %q %v", got, err)
	}
	other, _ := NewBox(RandomBytes(32))
	if _, err := other.Open(enc); err == nil {
		t.Fatal("opened with another key")
	}
	if b.Hash("a") != b.Hash("a") || b.Hash("a") == other.Hash("a") {
		t.Fatal("hash must be keyed and stable")
	}
	if !b.Verify(b.Sign("x", "y"), "x", "y") || b.Verify(b.Sign("x", "y"), "x", "z") {
		t.Fatal("sign/verify")
	}
	if _, err := ParseMasterKey("short"); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestPassword(t *testing.T) {
	h := HashPassword("hunter22")
	if !CheckPassword(h, "hunter22") || CheckPassword(h, "hunter23") || CheckPassword("garbage", "hunter22") {
		t.Fatal("password check")
	}
}
