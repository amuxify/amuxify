# amuxify.com

Plain static site: `index.html`, `style.css`, the logo, `CNAME` and
`.nojekyll`. No generator, no build step, no dependencies. It deploys to any
static host; GitHub Pages publishes it from `.github/workflows/pages.yml` on
every push to `main`.

Preview locally:

```sh
python3 -m http.server -d site 8000   # http://localhost:8000
```

The documentation is not copied here; the page links to `docs/` rendered on
GitHub. No analytics.
