# YUB WPanel Cloudflare entry

`wpanel-entry.js` serves the short installation endpoint at
`https://wpanel.zangyubin.top/install`.

The Worker does not serve repository `main`. It downloads the fixed Release's
`bootstrap.sh`, SHA-256 manifest, and Ed25519 signature, verifies both the
signature and content digest with the pinned YUB WPanel public key, checks the
embedded release identity, and only then returns the bootstrap to the client.

The `/` path redirects to the GitHub repository, `/healthz` is a non-sensitive
availability check, and every other path fails closed.

Deploy only after the matching fixed Release exists:

```bash
wrangler deploy --config deploy/cloudflare/wrangler.jsonc
```
