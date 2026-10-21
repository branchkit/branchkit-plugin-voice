#!/usr/bin/env python3
"""Generate the audio clips the engine's grammar tests decode, with macOS TTS.

The #[ignore] tests in src/engine.rs (offline_swap_verify) check the grammar
builder against real decodes: a command decodes to itself, a word the grammar
excludes cannot come out, an open tail decodes a union word. They need clips
that say "copy", "up", "scroll up" and so on, which used to live in a private
folder on one machine, so the tests never ran.

This tool makes them: each clip is synthesized with `say -o` (writes a file,
plays nothing) and decoded through `sherpa_commands probe` against a control
grammar holding every fixture word. The first voice and rate whose clip
decodes to exactly its own words is kept; TTS clarity varies a lot by voice
(Samantha's "up" decodes to nothing, Daniel's to UP). A clip no voice can
carry fails the run instead of making the tests fail for the wrong reason.

Output goes outside the repo: Apple's licence limits `say` output to personal,
non-commercial use, so these clips are generated, never committed. Kept clips
are reused until the probe binary or the model changes (recorded in
fixtures.json).

Usage (normally via `just engine-tests`):
  make_engine_fixtures.py --probe BIN --model DIR --out DIR
"""
import argparse
import hashlib
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

# file name -> phrase. The names are the ones src/engine.rs reads.
FIXTURES = {
    "copy_1.wav": "copy",
    "paste_1.wav": "paste",
    "up_1.wav": "up",
    "down_1.wav": "down",
    "left_1.wav": "left",
    "right_1.wav": "right",
    "scroll_up_1.wav": "scroll up",
    "cap_red_1.wav": "cap red",
}

# Tried in order. A mix of accents and engines, all installed with macOS.
VOICES = ["Daniel", "Samantha", "Karen", "Moira", "Tessa", "Fred"]
RATES = [150, 190]


def fingerprint(probe: Path, model: Path) -> str:
    """What a kept clip was verified against: the probe binary and the model."""
    h = hashlib.sha256()
    for f in [probe, model / "model.onnx", model / "tokens.txt"]:
        st = f.stat()
        h.update(f"{f}:{st.st_size}:{int(st.st_mtime)}".encode())
    return h.hexdigest()[:16]


def synth(path: Path, phrase: str, voice: str, rate: int) -> bool:
    out = subprocess.run(
        ["say", "-v", voice, "-r", str(rate), "-o", str(path),
         "--data-format=LEI16@16000", phrase],
        capture_output=True,
    )
    return out.returncode == 0 and path.exists()


def decode_all(probe: Path, model: Path, items: list) -> dict:
    """Decode every candidate in one probe run; id -> uppercase text."""
    req = {"model": str(model), "items": items}
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(req, f)
        req_path = f.name
    try:
        out = subprocess.run([str(probe), "probe", req_path],
                             capture_output=True, text=True)
    finally:
        os.unlink(req_path)
    if out.returncode != 0:
        sys.exit(f"error: probe failed: {out.stderr.strip()}")
    results = {}
    for line in out.stdout.splitlines():
        try:
            r = json.loads(line)
        except json.JSONDecodeError:
            continue
        results[r["id"]] = " ".join(r.get("text", "").split()).upper()
    return results


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--probe", required=True, help="sherpa_commands binary")
    ap.add_argument("--model", required=True, help="NeMo offline model dir")
    ap.add_argument("--out", required=True, help="fixtures dir (outside the repo)")
    args = ap.parse_args()

    probe, model, out = Path(args.probe), Path(args.model), Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    manifest_path = out / "fixtures.json"
    fp = fingerprint(probe, model)
    try:
        manifest = json.loads(manifest_path.read_text())
    except (OSError, json.JSONDecodeError):
        manifest = {}
    if manifest.get("fingerprint") != fp:
        manifest = {"fingerprint": fp, "clips": {}}

    todo = {n: p for n, p in FIXTURES.items()
            if n not in manifest["clips"] or not (out / n).exists()}
    if not todo:
        print(f"engine fixtures up to date ({len(FIXTURES)} clips in {out})")
        return

    control = sorted({w.upper() for p in FIXTURES.values() for w in p.split()})
    cand_dir = out / "candidates"
    cand_dir.mkdir(exist_ok=True)
    items, meta = [], {}
    for name, phrase in todo.items():
        for voice in VOICES:
            for rate in RATES:
                wav = cand_dir / f"{name[:-4]}__{voice}_{rate}.wav"
                if not wav.exists() and not synth(wav, phrase, voice, rate):
                    continue
                cid = f"{name}|{voice}|{rate}"
                items.append({"id": cid, "audio": str(wav), "words": control})
                meta[cid] = (name, phrase, voice, rate, wav)

    heard = decode_all(probe, model, items)
    missing = []
    for name, phrase in todo.items():
        pick = next(
            (m for cid, m in meta.items()
             if m[0] == name and heard.get(cid) == phrase.upper()),
            None,
        )
        if pick is None:
            missing.append(phrase)
            continue
        _, _, voice, rate, wav = pick
        (out / name).write_bytes(wav.read_bytes())
        manifest["clips"][name] = {"phrase": phrase, "voice": voice, "rate": rate}
        print(f"  {name:<18} {phrase!r:<12} {voice} @ {rate}")

    manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")
    if missing:
        sys.exit(f"error: no voice/rate decodes to itself for: {', '.join(missing)}")
    print(f"engine fixtures ready in {out}")


if __name__ == "__main__":
    main()
