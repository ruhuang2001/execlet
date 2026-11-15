"""Bound Docker fixture setup before scenarios; preserve pull diagnostics."""
import pathlib
import subprocess
import sys
import time

out = pathlib.Path(sys.argv[1])
image = "alpine:3.21"
def inspect_image(log):
    command = ["docker", "image", "inspect", image]
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=15)
    except subprocess.TimeoutExpired:
        log.write("image inspect exceeded 15 seconds\n")
        log.flush()
        return subprocess.CompletedProcess(command, 124, stdout="", stderr="")
    log.write(result.stdout + result.stderr)
    log.flush()
    return result


with (out / "image-setup.log").open("w") as log:
    inspected = inspect_image(log)
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
                inspected = inspect_image(log)
        if inspected.returncode != 0 and attempt < 2:
            time.sleep(attempt + 1)
    if inspected.returncode != 0:
        raise SystemExit(f"Docker fixture {image} unavailable; see {out / 'image-setup.log'}")
    (out / "fixture-image.json").write_text(inspected.stdout)
