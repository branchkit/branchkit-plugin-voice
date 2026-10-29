#!/usr/bin/env python3
"""Time whisper.cpp dictation: how long a release waits for its text.

    python3 scripts/whisper-latency.py [--stage PATH] [--clips DIR] ...

Dictation off macOS transcribes the whole recording when the key is
released, through `whisper_stage` (stages/whisper-stt). This times that
decode on 5, 15 and 30 s of speech for each whisper.cpp model plugin.json
declares, so the default model and the backend can be chosen from numbers.

1. Clips: 16 kHz mono 16-bit WAVs made with the system's text-to-speech
   (`say` on macOS, `espeak-ng` on Linux, System.Speech on Windows), or
   read from `--clips DIR`. One passage is spoken, repeated until it is
   long enough, and cut at each duration. `--save-clips DIR` keeps them
   (with clips.json, saying how they were made) so other machines can be
   timed on the same audio; `--clips-only` stops there.
2. Models: each `whisper.cpp-*` artifact in plugin.json, downloaded from
   its declared URL into `--models-dir` and checked against its sha256, or
   found there already (the CLI's layout, `<dir>/<name>/<file>`).
3. Timing: `whisper_stage probe`, the stage's offline decode, which runs
   the live stage's own decode function. Per model, one process: the model
   loads once, one warm-up decode, then `--runs` timed decodes of each clip.
   `decode_ms` is what the stage reports for one decode (state, encoder,
   decoder), the time between release and text, apart from voice's own
   text steps.

The thread count defaults to the live stage's own default (the CPUs this
process may run on, at most 4), which no seeded pipeline overrides; `--threads
2,4,8` times each. Under a CPU quota (a container's --cpus) the stage uses
fewer; pass --threads with the quota there. `--use-gpu off|both` also times a GPU-built stage (Metal
on macOS) on the CPU alone.

Prints a table, and writes `--json` and `--markdown` (a GitHub job summary)
when asked. Exit 0 when every decode succeeded, 1 otherwise. Standard
library only, so it runs on a bare CI runner.
"""

import argparse
import array
import hashlib
import json
import os
import platform
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.request
import wave
from pathlib import Path

VOICE = Path(__file__).resolve().parent.parent
RATE = 16000
DURATIONS = (5, 15, 30)
EXE = ".exe" if sys.platform == "win32" else ""

# A dictated paragraph: ordinary words, numbers and names, the kind of text
# dictation is for. Spoken once it runs about a minute, longer than the
# longest clip; it is repeated if a fast voice leaves it short.
PASSAGE = (
    "Hi Sam, thanks for sending the draft over on Tuesday. I read it twice "
    "and I think the second section is much stronger than the first. The "
    "numbers in the table look right to me, although the total for March "
    "should be four thousand two hundred and fifty, not four thousand five "
    "hundred. Could you check that before Friday? I also wonder whether we "
    "need the long paragraph about the history of the project, since most "
    "readers already know it. If we cut it, the whole thing fits on two "
    "pages, which is what the committee asked for. On the budget, I agree "
    "with Maria that the travel line is too low. We have three people "
    "flying to Denver in June, and the hotel alone will cost more than we "
    "set aside. Let's raise it by about twenty percent and explain why in a "
    "short note. Finally, the meeting next week has moved to Thursday at "
    "ten in the small conference room on the third floor. Bring a printed "
    "copy if you can, because the projector in that room has not worked "
    "since the spring. Talk soon, and thanks again for all the work."
)


# --- clips ----------------------------------------------------------------


def read_wav(path):
    """Samples of a 16-bit PCM WAV as (rate, list of ints), mixed to mono."""
    with wave.open(str(path), "rb") as w:
        if w.getsampwidth() != 2 or w.getcomptype() != "NONE":
            raise ValueError(f"{path}: want 16-bit PCM, got {w.getsampwidth() * 8}-bit {w.getcomptype()}")
        channels, rate = w.getnchannels(), w.getframerate()
        data = array.array("h", w.readframes(w.getnframes()))
    if sys.byteorder == "big":
        data.byteswap()
    if channels > 1:
        data = array.array("h", (sum(data[i:i + channels]) // channels for i in range(0, len(data), channels)))
    return rate, data.tolist()


def resample(samples, rate, to=RATE):
    """Linear resampling: good enough for a timing clip, and dependency-free."""
    if rate == to or not samples:
        return list(samples)
    n = int(len(samples) * to / rate)
    step = rate / to
    out = []
    last = len(samples) - 1
    for i in range(n):
        x = i * step
        j = int(x)
        frac = x - j
        a = samples[j]
        b = samples[j + 1] if j < last else a
        out.append(int(round(a + (b - a) * frac)))
    return out


def write_wav(path, samples, rate=RATE):
    data = array.array("h", samples)
    if sys.byteorder == "big":
        data.byteswap()
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(data.tobytes())


def cut(samples, durations, rate=RATE):
    """The first N seconds for each duration, the speech repeated to fill."""
    if not samples:
        raise ValueError("the speech is empty")
    need = max(durations) * rate
    filled = list(samples)
    while len(filled) < need:
        filled.extend(samples)
    return {d: filled[: d * rate] for d in durations}


def tts_engine():
    """The text-to-speech this machine has, by name, or None."""
    if sys.platform == "darwin" and shutil.which("say"):
        return "say"
    if sys.platform == "win32":
        return "system.speech"
    for name in ("espeak-ng", "espeak"):
        if shutil.which(name):
            return name
    return None


def ps_quote(value):
    """A PowerShell single-quoted string literal: each quote inside is
    doubled, the typographic ones PowerShell also reads as quotes included."""
    s = str(value)
    for q in "'‘’‚‛":
        s = s.replace(q, q + q)
    return "'" + s + "'"


def speak(engine, text, out):
    """Speaks `text` into the WAV `out` with `engine`. Nothing is played."""
    with tempfile.TemporaryDirectory() as tmp:
        txt = Path(tmp) / "passage.txt"
        txt.write_text(text, encoding="utf-8")
        if engine == "say":
            cmd = ["say", "-o", str(out), "--file-format=WAVE", "--data-format=LEI16@16000", "-f", str(txt)]
        elif engine in ("espeak-ng", "espeak"):
            cmd = [engine, "-w", str(out), "-f", str(txt)]
        elif engine == "system.speech":
            script = (
                "Add-Type -AssemblyName System.Speech;"
                "$s = New-Object System.Speech.Synthesis.SpeechSynthesizer;"
                "$f = New-Object System.Speech.AudioFormat.SpeechAudioFormatInfo(16000,"
                " [System.Speech.AudioFormat.AudioBitsPerSample]::Sixteen,"
                " [System.Speech.AudioFormat.AudioChannel]::Mono);"
                f"$s.SetOutputToWaveFile({ps_quote(out)}, $f);"
                f"$s.Speak([IO.File]::ReadAllText({ps_quote(txt)}));"
                "$s.Dispose()"
            )
            cmd = ["powershell", "-NoProfile", "-NonInteractive", "-Command", script]
        else:
            raise ValueError(f"unknown text-to-speech {engine!r}")
        subprocess.run(cmd, check=True, stdout=subprocess.DEVNULL, timeout=300)


def make_clips(out_dir, durations, engine):
    """Writes speech-<N>s.wav per duration and clips.json into out_dir."""
    out_dir.mkdir(parents=True, exist_ok=True)
    raw = out_dir / "passage-raw.wav"
    speak(engine, PASSAGE, raw)
    rate, samples = read_wav(raw)
    raw.unlink()
    clips = cut(resample(samples, rate), durations)
    names = {}
    for d, s in clips.items():
        name = f"speech-{d:02d}s.wav"
        write_wav(out_dir / name, s)
        names[name] = d
    meta = {"tts": engine, "made_on": host_info()["os"], "spoken_s": round(len(samples) / rate, 1), "clips": names, "text": PASSAGE}
    (out_dir / "clips.json").write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8")
    return meta


def load_clips(clip_dir):
    """The clips in a folder: clips.json's list when there is one, else every WAV."""
    meta_path = clip_dir / "clips.json"
    meta = json.loads(meta_path.read_text(encoding="utf-8")) if meta_path.is_file() else {"tts": "given", "clips": {}}
    names = list(meta.get("clips") or sorted(p.name for p in clip_dir.glob("*.wav")))
    clips = []
    for name in names:
        path = clip_dir / name
        rate, samples = read_wav(path)
        if rate != RATE:
            raise ValueError(f"{path}: want 16 kHz (the stage's input), got {rate} Hz")
        clips.append({"name": name, "path": str(path.resolve()), "audio_s": round(len(samples) / RATE, 2)})
    if not clips:
        raise ValueError(f"{clip_dir}: no WAV clips")
    clips.sort(key=lambda c: c["audio_s"])
    return meta, clips


# --- models -----------------------------------------------------------------


def declared_models(manifest):
    """plugin.json's whisper.cpp artifacts: name -> its parts."""
    artifacts = manifest.get("provides", {}).get("artifacts", {})
    return {name: a["parts"] for name, a in artifacts.items() if name.startswith("whisper.cpp-")}


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            h.update(block)
    return h.hexdigest()


def fetch_model(name, parts, models_dir, allow_fetch):
    """The model's folder under models_dir, downloading any part it lacks."""
    folder = models_dir / name
    for part in parts:
        if part.get("kind") != "http_file":
            raise ValueError(f"{name}: part kind {part.get('kind')!r} is not one this bench fetches")
        dest = folder / part["dest"]
        if dest.is_file():
            continue
        if not allow_fetch:
            raise FileNotFoundError(f"{dest} is missing and --no-fetch was given")
        folder.mkdir(parents=True, exist_ok=True)
        tmp = dest.with_name(dest.name + ".part")
        print(f"fetching {name}: {part['url']}", file=sys.stderr)
        with urllib.request.urlopen(part["url"]) as r, open(tmp, "wb") as f:
            shutil.copyfileobj(r, f, 1 << 20)
        got = sha256(tmp)
        if got != part["sha256"]:
            tmp.unlink()
            raise ValueError(f"{name}: sha256 {got} is not the declared {part['sha256']}")
        tmp.replace(dest)
    return folder


# --- timing -----------------------------------------------------------------


def live_threads():
    """The live stage's default thread count (whisper-stt's parse_stage_args,
    Rust's available_parallelism at most 4): the CPUs this process may run
    on, at most 4. Rust also lowers it to a cgroup CPU quota (a container's
    --cpus, a systemd CPUQuota=), which this does not try to reproduce:
    under one, pass --threads with the quota's CPU count."""
    try:
        n = len(os.sched_getaffinity(0))
    except AttributeError:
        n = os.cpu_count() or 1
    return max(1, min(n, 4))


def probe_request(model_dir, clips, runs, warmup, threads, use_gpu):
    """One probe request: warm-up decodes of the shortest clip, then each clip
    `runs` times. Item ids say which is which."""
    items = [{"id": f"warmup/{i}", "audio": clips[0]["path"]} for i in range(warmup)]
    for clip in clips:
        items += [{"id": f"{clip['name']}/{r}", "audio": clip["path"]} for r in range(runs)]
    req = {"model": str(model_dir), "language": "en", "threads": threads, "items": items}
    if use_gpu is not None:
        req["use_gpu"] = use_gpu
    return req


def summarize(lines, clips):
    """Per clip: the timed decodes, their median, the real-time factor, and the
    first run's text. Errors are collected, not raised."""
    by_clip = {c["name"]: [] for c in clips}
    texts, errors = {}, []
    for line in lines:
        name, _, run = line["id"].rpartition("/")
        if line.get("error"):
            errors.append(f"{line['id']}: {line['error']}")
            continue
        if name == "warmup" or name not in by_clip:
            continue
        by_clip[name].append(line["decode_ms"])
        if run == "0":
            texts[name] = line.get("text", "")
    rows = []
    for clip in clips:
        ms = by_clip[clip["name"]]
        if not ms:
            continue
        median = statistics.median(ms)
        rows.append({
            "clip": clip["name"],
            "audio_s": clip["audio_s"],
            "decode_ms": ms,
            "median_ms": round(median, 1),
            "min_ms": min(ms),
            "max_ms": max(ms),
            "rtf": round(median / 1000 / clip["audio_s"], 3),
            "text": texts.get(clip["name"], ""),
        })
    return rows, errors


def run_probe(stage, req):
    """Runs the probe on one request: its result lines and its wall time."""
    started = time.monotonic()
    # UTF-8 both ways, whatever the console's code page: the stage reads and
    # writes UTF-8 JSON, and a transcript may hold any character.
    # An hour is far past any pass (ten decodes of at most 30 s of audio), so
    # only a hung stage reaches it.
    proc = subprocess.run([str(stage), "probe"], input=json.dumps(req), capture_output=True,
                          encoding="utf-8", errors="replace", timeout=3600)
    wall_ms = (time.monotonic() - started) * 1000
    if proc.returncode != 0:
        raise RuntimeError(f"whisper_stage probe exited {proc.returncode}: {proc.stderr.strip()[-2000:]}")
    lines = [json.loads(line) for line in proc.stdout.splitlines() if line.strip()]
    return lines, wall_ms


# --- report -----------------------------------------------------------------


def cpu_model():
    try:
        if sys.platform == "darwin":
            return subprocess.run(["sysctl", "-n", "machdep.cpu.brand_string"], capture_output=True, text=True).stdout.strip()
        if sys.platform.startswith("linux"):
            info = Path("/proc/cpuinfo").read_text()
            for key in ("model name", "Model", "Hardware"):
                for line in info.splitlines():
                    if line.startswith(key):
                        return line.split(":", 1)[1].strip()
            return platform.processor() or "unknown"
        if sys.platform == "win32":
            out = subprocess.run(
                ["powershell", "-NoProfile", "-Command", "(Get-CimInstance Win32_Processor).Name"],
                capture_output=True, text=True,
            ).stdout.strip()
            return out or platform.processor()
    except OSError:
        pass
    return platform.processor() or "unknown"


def cpu_flags():
    """The SIMD features whisper.cpp's CPU path uses, where the OS lists them."""
    if not sys.platform.startswith("linux"):
        return None
    for line in Path("/proc/cpuinfo").read_text().splitlines():
        if line.startswith(("flags", "Features")):
            have = set(line.split(":", 1)[1].split())
            return sorted(have & {"avx", "avx2", "avx512f", "avx512_vnni", "avx_vnni", "fma", "f16c", "asimd", "asimddp", "sve", "i8mm"})
    return None


def load_average():
    """The 1-minute load average where the OS keeps one: other work on the
    machine slows every decode, so a number is read beside it."""
    try:
        return round(os.getloadavg()[0], 1)
    except (AttributeError, OSError):
        return None


def host_info():
    return {
        "os": f"{platform.system()} {platform.release()}",
        "arch": platform.machine(),
        "cpu": cpu_model(),
        "logical_cpus": os.cpu_count(),
        "cpu_flags": cpu_flags(),
        "load_avg": load_average(),
        "runner": os.environ.get("RUNNER_NAME") and f"{os.environ.get('ImageOS', '')} {os.environ.get('ImageVersion', '')}".strip(),
    }


# whisper-stt builds with Metal on macOS and for the CPU everywhere else
# (its Cargo.toml), so only macOS has a GPU to turn off.
HAS_GPU = sys.platform == "darwin"


def gpu_modes(choice):
    """The probe's use_gpu per timing pass: None leaves it to the build. Off
    macOS there is one pass whatever was asked, since both would be the CPU."""
    if not HAS_GPU:
        return [None]
    return {"build": [None], "off": [False], "both": [None, False]}[choice]


def backend_label(use_gpu):
    return "metal" if HAS_GPU and use_gpu is not False else "cpu"


def table(report):
    head = ["model", "backend", "threads", "audio s", "median ms", "min-max ms", "RTF"]
    rows = [[
        r["model"], r["backend"], str(r["threads"]), f"{r['audio_s']:.1f}",
        f"{r['median_ms']:.0f}", f"{r['min_ms']:.0f}-{r['max_ms']:.0f}", f"{r['rtf']:.2f}",
    ] for r in report["results"]]
    return head, rows


def text_table(report):
    head, rows = table(report)
    widths = [max(len(x) for x in col) for col in zip(head, *rows)]
    fmt = "  ".join(f"{{:<{w}}}" for w in widths)
    return "\n".join([fmt.format(*head)] + [fmt.format(*r) for r in rows])


def markdown(report):
    h = report["host"]
    head, rows = table(report)
    out = [
        f"### whisper.cpp dictation latency: {h['os']} {h['arch']}",
        "",
        f"CPU: {h['cpu']} ({h['logical_cpus']} logical)" + (f", flags: {' '.join(h['cpu_flags'])}" if h.get("cpu_flags") else "")
        + (f"; load average {h['load_avg']} before, {report['load_avg_end']} after" if h.get("load_avg") is not None else ""),
        f"Clips: {report['clips']['tts']} speech; {report['config']['warmup']} warm-up, {report['config']['runs']} timed runs each. "
        "RTF is median decode time over audio length (below 1 is faster than real time).",
        "",
        "| " + " | ".join(head) + " |",
        "|" + "|".join("---" for _ in head) + "|",
    ]
    out += ["| " + " | ".join(r) + " |" for r in rows]
    loads = ", ".join(f"{k}: {v / 1000:.1f} s" for k, v in report["load_ms"].items())
    out += ["", f"Model load and process start (wall time less the decodes): {loads}", ""]
    if report["errors"]:
        out += ["**Errors:**", ""] + [f"- {e}" for e in report["errors"]] + [""]
    return "\n".join(out)


# --- main -------------------------------------------------------------------


def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--stage", type=Path, default=VOICE / "stages" / "target" / "release" / f"whisper_stage{EXE}",
                   help="the whisper_stage binary (default: the stages workspace's release build)")
    p.add_argument("--clips", type=Path, help="time these 16 kHz mono WAVs instead of making clips")
    p.add_argument("--save-clips", type=Path, help="keep the clips made here, with clips.json")
    p.add_argument("--clips-only", action="store_true", help="make the clips (into --save-clips) and stop")
    p.add_argument("--durations", default=",".join(map(str, DURATIONS)), help="clip lengths in seconds (default 5,15,30)")
    p.add_argument("--model", action="append", help="a declared model name (repeatable; default: every whisper.cpp model) or a ggml file")
    p.add_argument("--models-dir", type=Path, default=default_models_dir(), help="where models are found or fetched to")
    p.add_argument("--no-fetch", action="store_true", help="refuse to download a missing model")
    p.add_argument("--runs", type=int, default=3)
    p.add_argument("--warmup", type=int, default=1)
    p.add_argument("--threads", help="comma-separated thread counts (default: the live stage's default)")
    p.add_argument("--use-gpu", choices=("build", "off", "both"), default="build",
                   help="build: as the stage was built (Metal on macOS); off: CPU only; both")
    p.add_argument("--json", type=Path, help="write the full report here")
    p.add_argument("--markdown", type=Path, help="append a markdown summary here (e.g. $GITHUB_STEP_SUMMARY)")
    return p.parse_args(argv)


def default_models_dir():
    if sys.platform == "win32":
        base = Path(os.environ.get("LOCALAPPDATA") or Path.home() / "AppData" / "Local")
    else:
        base = Path(os.environ.get("XDG_CACHE_HOME") or Path.home() / ".cache")
    return base / "branchkit-whisper-latency"


def resolve_models(args):
    """(label, model path) per model to time."""
    # UTF-8 explicitly: Windows would read it in the console's code page.
    declared = declared_models(json.loads((VOICE / "plugin.json").read_text(encoding="utf-8")))
    wanted = args.model or sorted(declared)
    out = []
    for name in wanted:
        if name in declared:
            out.append((name, fetch_model(name, declared[name], args.models_dir, not args.no_fetch)))
        elif Path(name).is_file():
            out.append((Path(name).stem, Path(name).resolve()))
        else:
            raise ValueError(f"{name!r} is neither a declared whisper.cpp model ({', '.join(sorted(declared))}) nor a file")
    return out


def main(argv=None):
    args = parse_args(argv)
    if args.clips_only and not args.save_clips:
        print("--clips-only keeps nothing without --save-clips DIR", file=sys.stderr)
        return 2
    # Clips made and not kept live in a folder removed at the end.
    with tempfile.TemporaryDirectory(prefix="whisper-latency-") as scratch:
        return bench(args, args.save_clips or Path(scratch))


def bench(args, clip_dir):
    durations = sorted({int(d) for d in args.durations.split(",") if d.strip()})

    if args.clips:
        clip_meta, clips = load_clips(args.clips)
    else:
        engine = tts_engine()
        if engine is None:
            print("no text-to-speech found (say, espeak-ng, System.Speech): pass --clips DIR", file=sys.stderr)
            return 1
        make_clips(clip_dir, durations, engine)
        clip_meta, clips = load_clips(clip_dir)
        print(f"clips: {engine} speech in {clip_dir}", file=sys.stderr)
    if args.clips_only:
        return 0

    if not args.stage.is_file():
        print(f"no whisper_stage at {args.stage}: build it (cargo build --release -p whisper-stt in stages/) or pass --stage", file=sys.stderr)
        return 1
    threads = list(dict.fromkeys(int(t) for t in args.threads.split(","))) if args.threads else [live_threads()]
    if not HAS_GPU and args.use_gpu != "build":
        print("this whisper_stage is built for the CPU only: one pass", file=sys.stderr)

    report = {
        "host": host_info(),
        "stage": str(args.stage),
        "clips": {k: v for k, v in clip_meta.items() if k != "text"} | {"files": clips},
        "config": {"runs": args.runs, "warmup": args.warmup, "threads": threads, "use_gpu": args.use_gpu},
        "results": [],
        "load_ms": {},
        "load_avg_end": None,
        "errors": [],
    }
    for label, model_path in resolve_models(args):
        for use_gpu in gpu_modes(args.use_gpu):
            for n in threads:
                backend = backend_label(use_gpu)
                req = probe_request(model_path, clips, args.runs, args.warmup, n, use_gpu)
                print(f"timing {label} ({backend}, {n} threads)...", file=sys.stderr)
                try:
                    lines, wall_ms = run_probe(args.stage, req)
                except (OSError, RuntimeError, ValueError, subprocess.TimeoutExpired) as e:
                    # One pass failing (a model that will not load, a crash)
                    # keeps every pass already timed, and the run exits 1.
                    report["errors"].append(f"{label} {backend} {n}t: {e}")
                    continue
                rows, errors = summarize(lines, clips)
                decoded = sum(line.get("decode_ms") or 0 for line in lines)
                report["load_ms"][f"{label} {backend} {n}t"] = round(wall_ms - decoded)
                report["errors"] += [f"{label} {backend} {n}t {e}" for e in errors]
                report["results"] += [{"model": label, "backend": backend, "threads": n} | r for r in rows]
    report["load_avg_end"] = load_average()

    h = report["host"]
    load = f", load average {h['load_avg']} before, {report['load_avg_end']} after" if h["load_avg"] is not None else ""
    print(f"{h['cpu']} ({h['logical_cpus']} logical), {h['os']} {h['arch']}{load}")
    print(text_table(report))
    for e in report["errors"]:
        print(f"error: {e}", file=sys.stderr)
    if args.json:
        args.json.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    if args.markdown:
        with open(args.markdown, "a", encoding="utf-8") as f:
            f.write(markdown(report) + "\n")
    return 1 if report["errors"] or not report["results"] else 0


if __name__ == "__main__":
    sys.exit(main())
