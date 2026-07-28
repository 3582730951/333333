package upstream

import "codex-account-pool/internal/bodysource"

func testBody(body []byte) bodysource.BodySource { return bodysource.Bytes(body) }
