---
name: web-crawler
description: Automated web crawling + scraping + recon workflow. Trigger: crawl, crawler, scrape, spider, 鐖櫕, 鎶撳彇, web crawl, site crawl, recon crawl.
---

# Web Crawler Workflow

Auto-chain without confirmation:

Phase 1 鈥?Discover:
  Spider the target domain. Extract all links, forms, endpoints, JS files.
  Identify: login pages, admin panels, API endpoints, upload forms.

Phase 2 鈥?Fingerprint:
  Tech stack: server, framework, CMS, JS libraries, CDN.
  Headers: security headers missing, cookie flags, CORS config.

Phase 3 鈥?Attack surface:
  `skills/web-pentest/scripts/sqli_scanner.py` 鈫?all form/query params
  `skills/web-pentest/scripts/xss_fuzzer.py` 鈫?all reflected inputs
  `skills/web-pentest/scripts/auth_bypass.py` 鈫?login pages, JWT tokens

Phase 4 鈥?Deep scan:
  Directory brute-force on discovered paths.
  API endpoint fuzzing for hidden endpoints.
  File upload testing on discovered upload forms.

Phase 5 鈥?Data extraction:
  All discovered data written to disk.
  Structured JSON output: {urls, forms, endpoints, vulns, tech}

Chat shows: [OK] 鈫?$PATH/crawl-results.json
