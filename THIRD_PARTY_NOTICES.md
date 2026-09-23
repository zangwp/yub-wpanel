# Third-Party Notices

YUB WPanel incorporates the following browser assets. The versions listed here
are the reviewed files stored in this repository and embedded in the released
binary.

- Alpine.js 3.13.7 — <https://github.com/alpinejs/alpine>
  - Copyright © 2019-2021 Caleb Porzio and contributors
- Chart.js 4.4.1 — <https://github.com/chartjs/Chart.js>
  - Copyright (c) 2014-2022 Chart.js Contributors
- @kurkle/color 0.3.2 (included by Chart.js) — <https://github.com/kurkle/color>
  - Copyright (c) 2018-2021 Jukka Kurkela
- Tailwind CSS 3.4.19 compiled output — <https://github.com/tailwindlabs/tailwindcss>
  - Copyright (c) Tailwind Labs, Inc.

Each component above is distributed under the MIT License:

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
the Software, and to permit persons to whom the Software is furnished to do so,
subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

YUB WPanel also embeds or statically links these components:

- Adminer 6.0.1 — <https://www.adminer.org/>
  - Copyright 2007 Jakub Vrana
  - Distributed by YUB WPanel under Adminer's Apache License, Version 2.0,
    option.
- Go 1.26.8 runtime and standard library — <https://go.dev/>
  - Copyright 2009 The Go Authors. All rights reserved.

The release asset `yub-wpanel-third-party-licenses.tar.gz` contains this notice,
the YUB WPanel Open Source License (`GPL-3.0-only`), the YUB WPanel project notice,
the Apache-2.0 license and attribution for the embedded Adminer 6.0.1 source,
the exact
LICENSE (and PATENTS file when present) from the Go toolchain used for the
release, and the exact top-level license/notice files from every non-standard
Go module linked into that release. Its root-level `RELEASE_VERSION` binds the
archive to the exact panel Release. The archive, its SHA-256 manifest, and its
Ed25519 signature are produced from the same tagged commit as the panel binary.
The signed installer rejects a mismatched version marker and places these
materials in `/usr/share/doc/yub-wpanel`.
