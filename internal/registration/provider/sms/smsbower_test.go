package sms

import (
	"strings"
	"testing"
)

func TestParseSMSBowerCountries_ObjectForm(t *testing.T) {
	// Object form keyed by numeric id (the documented shape).
	raw := `{"0":{"id":0,"rus":"Россия","eng":"Russia","chn":"俄罗斯"},"73":{"id":73,"eng":"Brazil","chn":"巴西"}}`
	out, err := parseSMSBowerCountries(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 countries, got %d", len(out))
	}
	byID := map[int]CountryInfo{}
	for _, c := range out {
		byID[c.ID] = c
	}
	if byID[73].Eng != "Brazil" || byID[73].Chn != "巴西" {
		t.Errorf("BR mismatch: %+v", byID[73])
	}
}

func TestParseSMSBowerCountries_BadKey(t *testing.T) {
	// Bad-key envelope must surface as an error, not an empty list.
	raw := `{"status":0,"message":"No access","data":[]}`
	if _, err := parseSMSBowerCountries(raw); err == nil {
		t.Fatal("expected error on bad-key envelope, got nil")
	}
}

func TestParseSMSBowerTopCountries_RankedCheapestPerCountry(t *testing.T) {
	// Documented shape: country -> partner -> {price,count}. Order of countries = success rank.
	raw := `{"usa":{"3170":{"price":0.12,"count":542},"4120":{"price":0.14,"count":301}},"canada":{"2211":{"price":0.11,"count":190}}}`
	out, err := parseSMSBowerTopCountries(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 countries, got %d", len(out))
	}
	// Order preserved = rank preserved.
	if out[0].Country != "usa" || out[0].Rank != 0 {
		t.Errorf("rank0 wrong: %+v", out[0])
	}
	if out[1].Country != "canada" || out[1].Rank != 1 {
		t.Errorf("rank1 wrong: %+v", out[1])
	}
	// usa has two partners; we keep the cheapest (0.12).
	if out[0].Price != 0.12 || out[0].Count != 542 {
		t.Errorf("usa cheapest partner wrong: %+v", out[0])
	}
}

func TestParseSMSBowerTopCountries_BadKey(t *testing.T) {
	raw := `{"status":0,"message":"No access","data":[]}`
	if _, err := parseSMSBowerTopCountries(raw); err == nil {
		t.Fatal("expected error on bad-key envelope")
	}
}

func TestParseSMSBowerAllPrices(t *testing.T) {
	raw := `{"4":{"dr":{"cost":0.025,"count":14}},"73":{"dr":{"cost":"0.045","count":"31"}},"15":{"tg":{"cost":0.2,"count":2}}}`
	out, err := parseSMSBowerAllPrices(raw, "dr")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out) != 2 || out[0].Country != "4" || out[1].Country != "73" {
		t.Fatalf("unexpected all-price catalog: %+v", out)
	}
	if out[1].Price != 0.045 || out[1].Count != 31 || out[1].Rank != 9999 {
		t.Fatalf("unexpected BR offer: %+v", out[1])
	}
}

func TestSMSBowerProvider_ServiceDefault(t *testing.T) {
	p := NewSMSBowerProvider("k", nil)
	if p.service != "dr" {
		t.Errorf("default service = %q, want dr", p.service)
	}
	if p.Name() != "smsbower" || p.Type() != "sms" {
		t.Errorf("name/type wrong: %s/%s", p.Name(), p.Type())
	}
}

func TestReadSMSProviderBodyIsLimited(t *testing.T) {
	body := readSMSProviderBody(strings.NewReader(strings.Repeat("x", smsProviderResponseBodyLimit*2)))
	if len(body) != smsProviderResponseBodyLimit {
		t.Fatalf("body length = %d, want %d", len(body), smsProviderResponseBodyLimit)
	}
}
