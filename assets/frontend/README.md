# Frontend source assets

This directory contains the Tailwind CSS source and its build configuration.
The generated stylesheet used by the embedded panel remains under
`static/css/`; release builds do not download frontend tooling or rebuild it.

From the repository root, run the pinned/reviewed Tailwind binary explicitly:

```sh
bin/tailwindcss -c assets/frontend/tailwind.config.js \
  -i assets/frontend/input.css -o static/css/main.css --minify
```

Review the generated `static/css/main.css` diff before committing it. Release
builds intentionally do not download a live Tailwind binary.
