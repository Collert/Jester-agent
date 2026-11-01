package ui

import (
	"fmt"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// wifiQR is the text a phone camera reads as "join this Wi-Fi" (the format Android and iOS both understand).
func wifiQR(ssid, password string) string {
	esc := strings.NewReplacer(`\`, `\\`, `;`, `\;`, `,`, `\,`, `:`, `\:`, `"`, `\"`)
	return "WIFI:T:WPA;S:" + esc.Replace(ssid) + ";P:" + esc.Replace(password) + ";;"
}

// qrSVG is a QR code as one SVG path (each dark run of a row is one rectangle) and its size in modules.
type qrSVG struct {
	Path string
	Size int
}

func makeQR(text string) qrSVG {
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return qrSVG{}
	}
	q.DisableBorder = true
	bits := q.Bitmap()
	var b strings.Builder
	for y, row := range bits {
		for x := 0; x < len(row); x++ {
			if !row[x] {
				continue
			}
			run := 1
			for x+run < len(row) && row[x+run] {
				run++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", x, y, run, run)
			x += run - 1
		}
	}
	return qrSVG{Path: b.String(), Size: len(bits)}
}
