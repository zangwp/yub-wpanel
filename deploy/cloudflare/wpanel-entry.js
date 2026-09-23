const RELEASE_VERSION = "v2.1.1";
const RELEASE_BASE = `https://github.com/zangwp/yub-wpanel/releases/download/${RELEASE_VERSION}`;
const BOOTSTRAP_NAME = "bootstrap.sh";
const PUBLIC_KEY_SPKI_B64 = "MCowBQYDK2VwAyEAc1EJlyDurxR/SJS8MTpUVsAbvSmtfUAatoabx/f5KvU=";
const MAX_BOOTSTRAP_BYTES = 4 * 1024 * 1024;
const MAX_MANIFEST_BYTES = 4 * 1024;
const SIGNATURE_BYTES = 64;

function base64Bytes(value) {
  const binary = atob(value);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

function hex(bytes) {
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

async function fetchAsset(name, maximumBytes) {
  const response = await fetch(`${RELEASE_BASE}/${name}`, {
    redirect: "follow",
    headers: { "User-Agent": "YUB-WPanel-Cloudflare-Entry" },
  });
  if (!response.ok) {
    throw new Error(`release asset ${name} returned HTTP ${response.status}`);
  }

  const declaredLength = Number(response.headers.get("content-length") || 0);
  if (declaredLength > maximumBytes) {
    throw new Error(`release asset ${name} exceeded its declared size limit`);
  }
  const bytes = new Uint8Array(await response.arrayBuffer());
  if (bytes.byteLength === 0 || bytes.byteLength > maximumBytes) {
    throw new Error(`release asset ${name} failed its size limit`);
  }
  return bytes;
}

async function verifiedBootstrap() {
  const [script, manifest, signature] = await Promise.all([
    fetchAsset(BOOTSTRAP_NAME, MAX_BOOTSTRAP_BYTES),
    fetchAsset(`${BOOTSTRAP_NAME}.sha256`, MAX_MANIFEST_BYTES),
    fetchAsset(`${BOOTSTRAP_NAME}.sha256.sig`, SIGNATURE_BYTES),
  ]);
  if (signature.byteLength !== SIGNATURE_BYTES) {
    throw new Error("release signature has an unexpected length");
  }

  const publicKey = await crypto.subtle.importKey(
    "spki",
    base64Bytes(PUBLIC_KEY_SPKI_B64),
    { name: "Ed25519" },
    false,
    ["verify"],
  );
  const signatureValid = await crypto.subtle.verify(
    { name: "Ed25519" },
    publicKey,
    signature,
    manifest,
  );
  if (!signatureValid) {
    throw new Error("release manifest signature verification failed");
  }

  const manifestText = new TextDecoder("utf-8", { fatal: true }).decode(manifest);
  const match = manifestText.match(/^([0-9a-f]{64})  bootstrap\.sh\n?$/);
  if (!match) {
    throw new Error("release manifest has an unexpected format");
  }
  const actualDigest = hex(new Uint8Array(await crypto.subtle.digest("SHA-256", script)));
  if (actualDigest !== match[1]) {
    throw new Error("bootstrap SHA-256 verification failed");
  }

  const scriptText = new TextDecoder("utf-8", { fatal: true }).decode(script);
  if (!scriptText.startsWith("#!/bin/bash\n") ||
      !scriptText.includes(`BOOTSTRAP_RELEASE_VERSION="${RELEASE_VERSION}"`) ||
      !scriptText.includes("BOOTSTRAP_DEFAULT_PREFER_CN=0")) {
    throw new Error("bootstrap release identity verification failed");
  }
  return script;
}

function responseHeaders() {
  return {
    "Cache-Control": "public, max-age=300",
    "Content-Disposition": `inline; filename="yub-wpanel-bootstrap-${RELEASE_VERSION}.sh"`,
    "Content-Type": "text/plain; charset=utf-8",
    "Referrer-Policy": "no-referrer",
    "X-Content-Type-Options": "nosniff",
    "X-YUB-WPanel-Release": RELEASE_VERSION,
  };
}

export default {
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("Method Not Allowed\n", {
        status: 405,
        headers: { Allow: "GET, HEAD" },
      });
    }

    if (url.pathname === "/") {
      return Response.redirect("https://github.com/zangwp/yub-wpanel", 302);
    }
    if (url.pathname === "/healthz") {
      return new Response(request.method === "HEAD" ? null : "ok\n", {
        headers: { "Cache-Control": "no-store", "Content-Type": "text/plain; charset=utf-8" },
      });
    }
    if (url.pathname !== "/install") {
      return new Response("Not Found\n", { status: 404 });
    }

    try {
      const script = await verifiedBootstrap();
      return new Response(request.method === "HEAD" ? null : script, {
        status: 200,
        headers: responseHeaders(),
      });
    } catch (error) {
      console.error("verified bootstrap unavailable", error);
      return new Response("YUB WPanel verified installer is temporarily unavailable.\n", {
        status: 503,
        headers: { "Cache-Control": "no-store", "Content-Type": "text/plain; charset=utf-8" },
      });
    }
  },
};
