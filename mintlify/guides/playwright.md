---
title: "Render a screenshot with Playwright"
description: "Render a trusted local HTML page in Chromium and retrieve its PNG via the file API."
---

Use the [Playwright preset](https://github.com/ruhuang2001/execlet/tree/main/examples/playwright)
to render a local page in an ordinary sandbox. Browser automation does not
require a separate Execlet API or a browser service.

## Run the offline example

From a source checkout with Docker and Python 3.10 or newer:

```sh
make browser-e2e
```

The first build downloads Node, Chromium, and browser dependencies. The
run itself has `network: "none"` and uses a fixed HTML fixture with no remote
assets. The suite verifies the network in Docker, then uses these existing APIs:

1. Create a sandbox from the local preset image.
2. Upload the page with `PUT /v1/sandboxes/{sandbox_id}/files`.
3. Start a command that runs headless Chromium and saves a PNG in the sandbox.
4. Download the image with `GET /v1/sandboxes/{sandbox_id}/files`. Compare the
   PNG dimensions and SHA-256 with the rendered artifact.
5. Delete the sandbox and verify that no run-owned containers remain.

The per-run directory under `artifacts/browser/` contains `screenshot.png`,
`result.json`, and logs. When the image build, command, download, or cleanup
fails, the suite exits nonzero and records the failure. Do not treat a PNG
from a previous run as evidence for a failed run.

## Decide what content to render

This preset runs Chromium as a non-root user with Chromium's own sandbox
disabled. Use it for the trusted local fixture, not arbitrary URLs or
untrusted sites. `network: "none"` isolates guest network access, but does
not add browser-level protection. Execlet also supports an opt-in
[filtered network policy](/operations/policy#filtered-egress); this example
does not exercise Chromium through that proxy.
