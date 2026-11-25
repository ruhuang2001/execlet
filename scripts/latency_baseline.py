"""Direct SDK workload for the operations harness. No image pulls or sleeps."""
import math
import statistics
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'sandbox-python' / 'src'))
from execlet import CancellationToken, Client, CreateSandboxRequest


def measure(domain, key, spec, samples):
    request = CreateSandboxRequest(**spec)
    rows = []
    with Client(domain=domain, api_key=key, max_retries=0) as client:
        for i in range(samples + 1):
            row = {}
            started = time.perf_counter_ns()
            sb = client.create_sandbox(request, cancel=CancellationToken(timeout=30))
            created = time.perf_counter_ns()
            try:
                assert sb.state == 'running' and sb.image == spec['image'], sb
                row['create_ready'] = created - started
                for echo in ((False, True) if i % 2 else (True, False)):
                    chunks = []
                    argv = ['/bin/echo', 'ok'] if echo else ['/bin/true']
                    token = CancellationToken(timeout=20)
                    t = time.perf_counter_ns()
                    ex = client.exec_stream(sb.id, argv, cancel=token,
                                            on_stdout=lambda s: chunks.append((time.perf_counter_ns(), s)))
                    elapsed = time.perf_counter_ns() - t
                    assert ex.exit_code == 0 and ex.termination_reason == 'exited' and not ex.output_truncated, ex
                    assert ''.join(s for _, s in chunks) == ex.stdout == ('ok\n' if echo else '') and not ex.stderr, ex
                    if echo:
                        row['echo_first_stdout'] = chunks[0][0] - t
                        row['echo_complete'] = elapsed
                    else:
                        row['true_complete'] = elapsed
            finally:
                t = time.perf_counter_ns()
                removed = client.kill_sandbox(sb.id, cancel=CancellationToken(timeout=30))
                end = time.perf_counter_ns()
                assert removed.id == sb.id and removed.state == 'killed', removed
            row.update(delete=end-t, workload=end-started)
            if i:
                rows.append({k: v / 1e6 for k, v in row.items()})

    summary = {}
    for name in rows[0]:
        values = sorted(row[name] for row in rows)
        summary[name] = dict(samples=len(values), p50_ms=statistics.median(values),
                             p95_ms=values[math.ceil(0.95 * len(values)) - 1])
    return dict(schema_version=1, unit='ms', concurrency=1, discarded_rounds=1,
                image_pull_measured=False, policy=True, journal=False, sdk_retries=0,
                sandbox_spec=spec, raw_rounds=rows, summary=summary)
