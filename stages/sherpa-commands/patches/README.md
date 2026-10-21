# sherpa-onnx patches

Two patches against sherpa-onnx v1.13.2, applied in order by
`../build-patched-sherpa.sh` (`.ps1` on Windows):

- `sherpa-onnx-v1.13.2-grammar-core.patch`: a grammar-constrained CTC decoder
  that composes its decoding graph lazily from a word list, so the grammar can
  change between utterances in milliseconds, with optional per-word weights.
- `sherpa-onnx-v1.13.2-branchkit-g.patch`, on top of it: building the graph
  from a structured grammar (a DAG of command phrases), split into a build
  step that can run on another thread and an install step that swaps the
  graph onto the decoder.

They modify sherpa-onnx, which is licensed under the Apache License 2.0, and
are distributed under the same license (`LICENSE` here, sherpa-onnx's own). The
rest of this plugin is MIT.
