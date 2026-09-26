---
title: amuxify
---

# The ingest gate for self-hosted media

Verify, sanitize, normalize, prove. One pass between "downloaded or purchased"
and "in the library", with a verdict your download client can act on.

```
curl -fsSL https://raw.githubusercontent.com/amuxify/amuxify/main/install.sh | sh
brew install --cask amuxify/tap/amuxify
docker run --rm -u 1000:1000 -v /srv/media:/data ghcr.io/amuxify/amuxify scan /data
```

Four verdicts: PASS, WARN, FAIL, BLOCK. Four profiles: homelab, anime, archive,
strict. Ten safety guarantees, each backed by a test.

Documentation lives in the repository under `docs/` and is rendered here.
