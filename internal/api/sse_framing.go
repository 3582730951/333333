package api

import "bytes"

// sseFrameBoundary returns the earliest complete SSE frame delimiter. Both LF
// and CRLF are valid on the wire, and either delimiter may be split across reads.
// Accept mixed newline pairs as well: reverse proxies can normalize one line at a
// time while forwarding an otherwise valid event stream.
func sseFrameBoundary(raw []byte) (int, int) {
	boundary, separatorLen := -1, 0
	for _, separator := range [][]byte{
		[]byte("\r\n\r\n"),
		[]byte("\n\n"),
		[]byte("\r\n\n"),
		[]byte("\n\r\n"),
	} {
		if index := bytes.Index(raw, separator); index >= 0 && (boundary < 0 || index < boundary || (index == boundary && len(separator) > separatorLen)) {
			boundary, separatorLen = index, len(separator)
		}
	}
	return boundary, separatorLen
}
