package sms

import "io"

const smsProviderResponseBodyLimit = 256 * 1024

func readSMSProviderBody(body io.Reader) []byte {
	raw, _ := io.ReadAll(io.LimitReader(body, smsProviderResponseBodyLimit))
	return raw
}
