#!/usr/bin/env python3
"""Export bpe.vocab (one "token<space>score" per line) from a sentencepiece
bpe.model.

The Direction 2 in-process grammar builder (the grammar-provider patch-overlay)
uses simple-sentencepiece to encode user-coined words that are absent from the
model's lexicon.txt. simple-sentencepiece consumes this token+score vocab, NOT
the bpe.model protobuf — so we export it once, next to bpe.model in the model
dir. First-party command words come from lexicon.txt and never hit this path;
bpe.vocab is the fallback for novel/coined words.

Deps: sentencepiece (see requirements.txt). Run it when converting a model;
the result is published beside the model (the voice manifest fetches it).

Usage:
  export_bpe_vocab.py --model-dir <dir>   # <dir>/bpe.model -> <dir>/bpe.vocab
"""
import argparse
from pathlib import Path

import sentencepiece as spm


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model-dir", required=True, help="dir holding bpe.model")
    args = ap.parse_args()

    d = Path(args.model_dir)
    model = d / "bpe.model"
    if not model.is_file():
        raise SystemExit(f"export_bpe_vocab: {model} not found")

    sp = spm.SentencePieceProcessor(model_file=str(model))
    out = d / "bpe.vocab"
    with open(out, "w", encoding="utf-8") as f:
        for i in range(sp.get_piece_size()):
            f.write(f"{sp.id_to_piece(i)} {sp.get_score(i)}\n")
    print(f"wrote {out} ({sp.get_piece_size()} pieces)")


if __name__ == "__main__":
    main()
