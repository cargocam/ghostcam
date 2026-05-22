package main

import "testing"

func TestParseMmcliOutput(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantRAT    string
		wantSigPct uint8
	}{
		{
			name:       "empty",
			in:         "",
			wantRAT:    "",
			wantSigPct: 0,
		},
		{
			name: "lte happy path",
			in: `  Status   |             state: connected
           |       access tech: lte
           |    signal quality: 80% (recent)`,
			wantRAT:    "LTE",
			wantSigPct: 80,
		},
		{
			name: "wcdma",
			in: `Status: access tech: umts
signal quality: 42%`,
			wantRAT:    "UMTS",
			wantSigPct: 42,
		},
		{
			name: "5g NSA dual-connect prefers 5g",
			in: `access tech: lte, 5gnr
signal quality: 65% (cached)`,
			wantRAT:    "5G_NSA",
			wantSigPct: 65,
		},
		{
			name: "5g NSA reversed order still prefers 5g",
			in: `access tech: 5gnr, lte
signal quality: 1%`,
			wantRAT:    "5G_NSA",
			wantSigPct: 1,
		},
		{
			name: "gsm fallback",
			in: `access tech: gsm
signal quality: 12% (recent)`,
			wantRAT:    "GSM",
			wantSigPct: 12,
		},
		{
			name: "missing signal line",
			in: `access tech: lte
ip address: 10.0.0.1`,
			wantRAT:    "LTE",
			wantSigPct: 0,
		},
		{
			name: "missing access tech line",
			in: `state: connected
signal quality: 55%`,
			wantRAT:    "",
			wantSigPct: 55,
		},
		{
			name: "signal quality 100",
			in: `access tech: lte
signal quality: 100% (recent)`,
			wantRAT:    "LTE",
			wantSigPct: 100,
		},
		{
			name: "signal quality out of range ignored",
			in: `access tech: lte
signal quality: 250% (corrupt)`,
			wantRAT:    "LTE",
			wantSigPct: 0,
		},
		{
			name: "extra whitespace tolerated",
			in: `   access tech   :   lte
   signal quality   :   77 %   (recent)`,
			wantRAT:    "LTE",
			wantSigPct: 77,
		},
		{
			name: "unknown rat passes through uppercased",
			in: `access tech: 6gnr
signal quality: 50%`,
			wantRAT:    "6GNR",
			wantSigPct: 50,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMmcliOutput(tc.in)
			if got.RAT != tc.wantRAT {
				t.Errorf("RAT = %q, want %q", got.RAT, tc.wantRAT)
			}
			if got.SigPct != tc.wantSigPct {
				t.Errorf("SigPct = %d, want %d", got.SigPct, tc.wantSigPct)
			}
		})
	}
}
