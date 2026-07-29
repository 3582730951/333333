# Console Asset Policy

## Vendor Logo Register

The admin console uses vendor marks only to identify configured providers in
operator UI. Do not use these assets for endorsement, marketing claims, or page
decoration.

| Vendor | Asset | Provenance |
| --- | --- | --- |
| OpenAI | `web-spa/src/assets/vendors/openai-blossom.svg` | OpenAI brand resources: https://openai.com/brand/ |
| Claude | `web-spa/src/assets/vendors/anthropic.svg` | Claude product identity: https://claude.com/ |
| Anthropic | `web-spa/src/assets/vendors/anthropic.svg` | Anthropic brand identity: https://www.anthropic.com/ |

When adding a vendor logo, add the source URL here, keep the asset under
`web-spa/src/assets/vendors/`, and render it through `VendorLogo.jsx` so sizing,
fallbacks, and accessibility stay consistent.
