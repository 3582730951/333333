---
name: phishing-kit
description: Phishing toolkit 鈥?page cloning, email spoofing, credential harvesting, pretext generation. Trigger: phishing, phish, credential harvest, spoof email, clone page, 閽撻奔, 浼€犻〉闈?
---

# Phishing Kit

Automated phishing infrastructure generation.

## Workflow
1. Clone target page 鈫?extract HTML/CSS/JS 鈫?rewrite form actions
2. Configure credential capture 鈫?email/webhook delivery
3. Generate pretext email 鈫?spoof sender 鈫?embed tracking pixel
4. Deploy landing page 鈫?ngrok/serveo/VPS 鈫?test capture
5. Optional: MFA token capture via reverse proxy (evilginx-style)

## Tools
- page_cloner.py: Clone target login page with form rewriting
- pretext_gen.py: Generate convincing email templates
- spoof_tools.py: Email header manipulation and SPF/DKIM analysis

## Output
Write cloned page + capture server + email templates to disk.
