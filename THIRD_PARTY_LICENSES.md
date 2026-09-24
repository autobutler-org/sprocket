# Third party licenses

Every module sprocket depends on, what it is licensed under, and the notices it
carries from the work it came from. sprocket itself is MIT No Attribution; see
[LICENSE](LICENSE).

## github.com/gen2brain/h265

Version v0.2.3. A pure-Go HEVC decoder and encoder. sprocket imports
`github.com/gen2brain/h265/hevc` to decode a single keyframe.

Licensed MIT. Its `LICENSE` file, verbatim:

```
MIT License

Copyright (c) 2025 roticv
Copyright (c) 2026 Karpeles Lab Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

### Work it came from

The module's own README states: "The decoder is a port of the pure-Rust
[rust_h265](https://github.com/roticv/rust_h265) and
[oxideav-h265](https://github.com/OxideAV/oxideav-h265) decoders and carries
their notices." Those notices are the two copyright lines above. There is no
separate notice file in the module, and nothing else is vendored into it: the
module is `LICENSE`, `README.md`, `go.mod`, and the `hevc` and `heic` packages,
with no bundled binary, WASM module, or third-party source tree.

The two upstream projects, as their repositories state them:

| Project | License | Copyright line |
| --- | --- | --- |
| [roticv/rust_h265](https://github.com/roticv/rust_h265) | `MIT OR Apache-2.0` (Cargo.toml), with `LICENSE-MIT` and `LICENSE-APACHE` in the repository | `Copyright (c) 2025 roticv` |
| [OxideAV/oxideav-h265](https://github.com/OxideAV/oxideav-h265) | MIT | `Copyright (c) 2026 Karpelès Lab Inc.` |

The copyright lines in `gen2brain/h265`'s own `LICENSE` match these, except that
it writes "Karpeles" where the upstream file writes "Karpelès".

## github.com/rcarmo/go-264

Version v0.0.0-20260918175710-241fd4164612, a pseudo-version: the repository has
no release tags, so the module is pinned by commit and by hash in `go.sum`. A
pure-Go H.264 decoder. sprocket imports `github.com/rcarmo/go-264/decode` to
decode a single keyframe, and only under the `h264` build tag, so a build
without that tag links none of it.

Licensed MIT. Its `LICENSE` file, verbatim:

```
MIT License

Copyright (c) 2026 Rui Carmo

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

### Work it came from

The module carries a `THIRD_PARTY_NOTICES.md`, and everything it attributes is
in the audio packages, which sprocket does not import. It opens: "The entire
go-264 project is distributed under the MIT License, copyright (c) 2026 Rui
Carmo. Imported MIT material also retains the notices below." The notices below
it are these:

| Part of the module | Upstream | License | Copyright line |
| --- | --- | --- | --- |
| `audio/aac` | [OxideAV/oxideav-aac](https://github.com/OxideAV/oxideav-aac) at `7dcb2f4a9e6f7ccfa6b199342aeb95861dc57885` | MIT | `Copyright (c) 2026 Karpelès Lab Inc.` |
| `audio/ac3` | [PasyDev/ac3psy](https://github.com/PasyDev/ac3psy) at `ebdd1d3d6cf80690d1c7e648231f7d603ca33f97` | MIT | `Copyright (c) 2026 PCPX` |

A third section lists test recordings that are referenced rather than bundled:
PolyAI's MINDS-14 under CC-BY-4.0 and a pyannote tutorial recording under its
own MIT notice. Those are fixtures in the module's own tests, not code.

The video decoder is presented as the author's own work, with no upstream
attributed to it, and the module ships no patent notice of any kind.

## golang.org/x/image

Version v0.46.0. The Go project's supplemental image packages. sprocket imports
`golang.org/x/image/vp8` to decode a single VP8 keyframe. It is pure Go, has no
dependencies of its own, and is maintained by the Go team.

Licensed BSD 3-Clause. Its `LICENSE` file, verbatim:

```
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

### Work it came from

The `vp8` package is the Go project's own work. The module carries a `PATENTS`
file, which is the Go project's standard patent grant, and vendors no
third-party source tree.

## github.com/gen2brain/gav1d

Version v0.2.5. A pure-Go AV1 decoder and encoder. sprocket imports
`github.com/gen2brain/gav1d/av1` to decode a single keyframe. It is cgo-free:
its SIMD is hand-written Go assembly for amd64, arm64, and riscv64, chosen at
run time, and a `noasm` build tag leaves pure Go everywhere. `CGO_ENABLED=0`
builds of sprocket for linux/arm64 and darwin/arm64 link it cleanly.

Licensed BSD 2-Clause. Its license file is named `COPYING`, not `LICENSE`, and
reads verbatim:

```
Copyright © 2018-2025, VideoLAN and dav1d authors
Copyright (c) 2016, Alliance for Open Media
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE LIABLE FOR
ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
(INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND
ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

### Work it came from

The two copyright lines above are the attribution. The decoder is a port of
[dav1d](https://code.videolan.org/videolan/dav1d), VideoLAN's AV1 decoder, which
carries the first line; the encoder's forward transform is a port of
[libaom](https://aomedia.googlesource.com/aom/), which carries the second.
sprocket imports the decoder only. Both upstream projects are BSD 2-Clause, and
the module vendors no source tree of its own: it is `COPYING`, `PATENTS`,
`README.md`, `go.mod`, and the `av1` and `avif` packages, with no bundled
binary or WASM module.

The module also carries a `PATENTS` file, which is the Alliance for Open Media
Patent License 1.0. That is a grant rather than a disclaimer, and it is the
same one every AV1 implementation ships. The README states: "This project is an
implementation of a codec. It gives you no special rights on the AV1 patents."

## Patents

`gen2brain/h265`'s README says, in its own words:

> This software implements a decoder and an encoder. It gives you no special
> rights on the HEVC patents. HEVC is covered by patents held by several pools
> and by unpooled holders; if you distribute or use this software you may need a
> licence from them.

That applies to sprocket too, and it is not specific to this dependency: no
software license grants patent rights in HEVC or H.264, whoever wrote the
decoder. `rcarmo/go-264` says nothing on the subject, so sprocket says it for
both codecs in the [README](README.md).
