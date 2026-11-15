"""Bound Docker fixture setup before scenarios; preserve pull diagnostics."""
import pathlib
import subprocess
import sys
import time

out = pathlib.Path(sys.argv[1])
image = "alpine:3.21"
with (out / "image-setup.log").open("w") as log:
    inspected = subprocess.run(["docker", "image", "inspect", image], capture_output=True, text=True, timeout=15)
    log.write(inspected.stdout + inspected.stderr)
    for attempt in range(3):
        if inspected.returncode == 0:
            break
        log.write(f"pull attempt {attempt + 1}/3: {image}\n")
        log.flush()
        try:
            pulled = subprocess.run(["docker", "pull", image], stdout=log, stderr=log, text=True, timeout=120)
        except subprocess.TimeoutExpired:
            log.write("pull exceeded 120 seconds\n")
        else:
            if pulled.returncode == 0:
                inspected = subprocess.run(["docker", "image", "inspect", image], capture_output=True, text=True, timeout=15)
                log.write(inspected.stdout + inspected.stderr)
        if inspected.returncode != 0 and attempt < 2:
            time.sleep(attempt + 1)
    if inspected.returncode != 0:
        raise SystemExit(f"Docker fixture {image} unavailable; see {out / 'image-setup.log'}")
    (out / "fixture-image.json").write_text(inspected.stdout)
