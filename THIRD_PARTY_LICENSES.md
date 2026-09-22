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

## Patents

`gen2brain/h265`'s README says, in its own words:

> This software implements a decoder and an encoder. It gives you no special
> rights on the HEVC patents. HEVC is covered by patents held by several pools
> and by unpooled holders; if you distribute or use this software you may need a
> licence from them.

That applies to sprocket too, and it is not specific to this dependency: no
software license grants patent rights in HEVC or H.264, whoever wrote the
decoder.
