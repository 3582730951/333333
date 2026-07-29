package pipeline

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// registrarBaseEnv intentionally excludes server credentials and generic proxy
// variables. Registrar workers receive only the specific provider and egress
// values required for their job.
func registrarBaseEnv() []string {
	allowed := map[string]struct{}{
		"HOME": {}, "LANG": {}, "LC_ALL": {}, "LOGNAME": {}, "NODE_EXTRA_CA_CERTS": {},
		"PATH": {}, "PYTHONPATH": {}, "SSL_CERT_DIR": {}, "SSL_CERT_FILE": {}, "TEMP": {},
		"TMP": {}, "TMPDIR": {}, "TZ": {}, "USER": {}, "XDG_CACHE_HOME": {}, "XDG_RUNTIME_DIR": {},
	}
	out := make([]string, 0, len(allowed))
	for _, entry := range os.Environ() {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		if _, ok := allowed[key]; ok {
			out = append(out, entry)
		}
	}
	return out
}

func (p *Pipeline) runRegistrarCommand(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	capacity := int(p.outputCap(ctx))
	stdout := newCappedBuffer(capacity)
	stderr := newCappedBuffer(capacity)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	isolateSubprocess(cmd)
	err := cmd.Run()
	terminateSubprocessGroup(cmd)
	return stdout.Bytes(), err
}
