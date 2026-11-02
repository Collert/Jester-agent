package device

import "testing"

func TestFullAvg10(t *testing.T) {
	text := "some avg10=41.20 avg60=30.02 avg300=12.50 total=123456\nfull avg10=27.85 avg60=19.01 avg300=8.00 total=98765\n"
	if n, ok := fullAvg10(text); !ok || n != 27.85 {
		t.Errorf("fullAvg10 = %v, %v; want 27.85", n, ok)
	}
	if _, ok := fullAvg10("some avg10=1.00 avg60=0.00 avg300=0.00 total=1\n"); ok {
		t.Error("no full line: not ok")
	}
}
