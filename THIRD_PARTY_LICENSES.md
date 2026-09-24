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
