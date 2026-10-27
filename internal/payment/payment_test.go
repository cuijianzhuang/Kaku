package payment

import (
	"crypto/md5"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
)

func TestSignAndCheckout(t *testing.T) {
	v := url.Values{"pid": {"1000"}, "money": {"1.00"}, "name": {"x"}, "empty": {""}}
	want := md5.Sum([]byte("money=1.00&name=x&pid=1000" + "key"))
	if got := Sign(v, "key"); got != hex.EncodeToString(want[:]) {
		t.Fatal(got)
	}
	v.Set("sign", Sign(v, "key"))
	if !Verify(v, "key") || Verify(v, "other") {
		t.Fatal("verify")
	}
	e := &EPay{BaseURL: "https://pay.example.com/submit.php", PID: "1000", Key: "key"}
	u, err := e.CheckoutURL("alipay", "K1", "商品", 1990, "https://s/notify", "https://s/return")
	if err != nil || !strings.HasPrefix(u, "https://pay.example.com/submit.php?") || !strings.Contains(u, "money=19.90") {
		t.Fatalf("checkout %s %v", u, err)
	}
	parsed, _ := url.Parse(u)
	if !Verify(parsed.Query(), "key") {
		t.Fatal("checkout signature invalid")
	}
	if (&EPay{BaseURL: "http://pay.example.com", PID: "1", Key: "k"}).Ready() {
		t.Fatal("plain http accepted")
	}
}

func TestDecimal(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "1": 100, "1.5": 150, "19.90": 1990} {
		if got, err := ParseDecimal(in, 2); err != nil || got != want {
			t.Fatalf("%s → %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "1.234", "-1", "1e3", "1.", ".5"} {
		if _, err := ParseDecimal(bad, 2); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if Decimal(1990, 2) != "19.90" || Trimmed(6_800_000, 6) != "6.8" || Trimmed(300, 2) != "3" {
		t.Fatal("format")
	}
}
