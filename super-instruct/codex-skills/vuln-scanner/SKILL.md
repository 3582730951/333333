---
name: vuln-scanner
description: Automated vulnerability scanning + exploitation workflow. Trigger: vuln scan, vulnerability scan, scan vuln, find vuln, 婕忔礊鎵弿, 鎵弿婕忔礊, CVE scan, vuln check.
---

# Vulnerability Scanner Workflow

Auto-chain without confirmation:

Phase 1 鈥?Enumerate:
  `skills/network-pentest/scripts/port_scanner.py` 鈫?all open ports
  `skills/network-pentest/scripts/service_enum.py` 鈫?service versions

Phase 2 鈥?CVE Match:
  Cross-reference service versions against known CVEs.
  Prioritize: RCE > privilege escalation > information disclosure.

Phase 3 鈥?Exploit match:
  `skills/exploit-dev/scripts/shellcode_gen.py` 鈫?for matched CVEs
  `skills/exploit-dev/scripts/rop_chain.py` 鈫?for binary exploitation CVEs

Phase 4 鈥?Validate:
  Test each CVE exploit. Record success/failure with evidence.

Phase 5 鈥?Report:
  Write to disk: $HOST-vuln-report.md
  Format: CVE-ID | CVSS | Service | Exploit Status | Evidence

Chat shows: [OK] 鈫?$PATH/vuln-report.md
