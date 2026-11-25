import json
import unittest
from contextlib import contextmanager
from io import BytesIO
from unittest.mock import patch

from execlet import Client, CreateSandboxRequest, CreateVolumeRequest, Error, Volume, VolumeMount


CAPABILITIES = "/v1/capabilities"
SANDBOXES = "/v1/sandboxes"
VOLUMES = "/v1/volumes"
SIZE = 1 << 34
VOLUME = {"id": "vol_a/b?c", "size_bytes": SIZE, "created_at": "2026-11-24T12:00:00Z"}
MOUNTS = [
    {"volume_id": "vol_a", "path": "/mnt/data"},
    {"volume_id": "vol_b", "path": "/mnt/input", "read_only": True},
    {"volume_id": "vol_c", "path": "/mnt/output", "read_only": False},
]


class VolumeTests(unittest.TestCase):
    def setUp(self):
        self.client = Client(max_retries=1)
        self.addCleanup(self.client.close)
        self.calls = []
        self.replies = {}

        @contextmanager
        def exchange(method, path, *, body, **kwargs):
            self.calls.append((method, path, json.loads(body) if body is not None else None))
            reply = self.replies[method, path].pop(0)
            if isinstance(reply, Error):
                raise reply
            status, data = reply
            response = BytesIO(json.dumps(data).encode() if data is not None else b"")
            response.status = status
            response.getheader = lambda name, default: default
            yield response

        exchange_patch = patch.object(self.client, "_exchange", exchange)
        exchange_patch.start()
        self.addCleanup(exchange_patch.stop)

    def test_volume_operations(self):
        item = VOLUMES + "/vol_a%2Fb%3Fc"
        self.replies.update({
            ("GET", CAPABILITIES): [(200, {"capabilities": ["volumes.v1"]})] * 4,
            ("POST", VOLUMES): [(201, VOLUME)],
            ("GET", VOLUMES): [(200, {"volumes": [VOLUME]})],
            ("GET", item): [(200, VOLUME)],
            ("DELETE", item): [(204, None)],
        })
        expected = Volume(**VOLUME)
        self.assertEqual(self.client.create_volume(CreateVolumeRequest(size_bytes=SIZE)), expected)
        self.assertEqual(self.client.list_volumes(), [expected])
        self.assertEqual(self.client.get_volume(VOLUME["id"]), expected)
        self.assertIsNone(self.client.delete_volume(VOLUME["id"]))
        self.assertEqual(self.calls, [
            ("GET", CAPABILITIES, None), ("POST", VOLUMES, {"size_bytes": SIZE}),
            ("GET", CAPABILITIES, None), ("GET", VOLUMES, None),
            ("GET", CAPABILITIES, None), ("GET", item, None),
            ("GET", CAPABILITIES, None), ("DELETE", item, None),
        ])

    def test_create_sandbox_with_mounts_batches_capabilities(self):
        snapshot = {"id": "sbx_a", "state": "running", "mounts": MOUNTS}
        self.replies.update({
            ("GET", CAPABILITIES): [(200, {"capabilities": [
                "volumes.v1", "sandbox.runtime.v1", "execution.env-cwd.v1", "execution.concurrent.v1",
            ]})],
            ("POST", SANDBOXES): [(201, snapshot)],
            ("GET", SANDBOXES + "/sbx_a"): [(200, snapshot)],
        })
        mounts = [VolumeMount(**mount) for mount in MOUNTS]
        created = self.client.create_sandbox(CreateSandboxRequest(
            mounts=mounts, runtime="runc", env={"A": "B"}, max_concurrent_executions=2,
        ))
        self.assertEqual(created.mounts, mounts)
        self.assertEqual(self.client.get_sandbox(created.id).mounts, mounts)
        self.assertEqual(self.calls, [
            ("GET", CAPABILITIES, None),
            ("POST", SANDBOXES, {"mounts": MOUNTS, "runtime": "runc", "env": {"A": "B"}, "max_concurrent_executions": 2}),
            ("GET", SANDBOXES + "/sbx_a", None),
        ])

    def test_unsupported_volumes_fail_before_dispatch(self):
        operations = (
            lambda: self.client.create_volume(CreateVolumeRequest(size_bytes=SIZE)),
            self.client.list_volumes,
            lambda: self.client.get_volume("vol_a"),
            lambda: self.client.delete_volume("vol_a"),
            lambda: self.client.create_sandbox(CreateSandboxRequest(
                mounts=[VolumeMount(**MOUNTS[0])], env={"A": "B"},
            )),
        )
        for reply in [(404, {"code": "not_found"}), (200, {"capabilities": ["execution.env-cwd.v1"]})]:
            for operation in operations:
                self.calls.clear()
                self.replies["GET", CAPABILITIES] = [reply]
                with self.assertRaises(Error) as caught:
                    operation()
                self.assertEqual(caught.exception.code, "incompatible_server")
                self.assertEqual(self.calls, [("GET", CAPABILITIES, None)])
        self.calls.clear()
        self.replies["POST", SANDBOXES] = [(201, {"id": "sbx_a", "state": "running"})]
        self.assertIsNone(self.client.create_sandbox(CreateSandboxRequest(mounts=[])).mounts)
        self.assertEqual(self.calls, [("POST", SANDBOXES, {})])

    def test_validation_precedes_mutation(self):
        mounts = [VolumeMount(**MOUNTS[0]), VolumeMount(volume_id="vol_b", path="/mnt/data/child")]
        for operation in (
            lambda: self.client.create_volume(CreateVolumeRequest(size_bytes=True)),
            lambda: self.client.create_volume(CreateVolumeRequest(size_bytes=SIZE + 1)),
            lambda: self.client.create_sandbox(CreateSandboxRequest(mounts=mounts)),
            lambda: self.client.delete_volume(".."),
        ):
            with self.assertRaises(Error) as caught:
                operation()
            self.assertEqual(caught.exception.code, "invalid_request")
        self.assertEqual(self.calls, [])

    def test_mount_path_boundary(self):
        path = "/mnt/" + "x/" * 2045 + "x"
        self.replies["GET", CAPABILITIES] = [(200, {"capabilities": ["volumes.v1"]})]
        self.replies["POST", SANDBOXES] = [(201, {"id": "sbx_a", "state": "running"})]
        mount = VolumeMount(volume_id="vol_a", path=path + "x")
        req = CreateSandboxRequest(mounts=[mount])
        with self.assertRaises(Error) as caught:
            self.client.create_sandbox(req)
        self.assertEqual(caught.exception.code, "invalid_request")
        self.assertEqual(self.calls, [])
        mount.path = path
        self.client.create_sandbox(req)
        self.assertEqual(self.calls[-1], ("POST", SANDBOXES, {"mounts": [mount.to_dict()]}))

    def test_delete_only_retries_unavailable(self):
        item = VOLUMES + "/vol_a"
        missing = (404, {"code": "not_found"})
        for replies, code, count in [
            ([Error("transport", "response lost"), missing], "transport", 1),
            ([missing], "not_found", 1),
            ([(504, {"code": "gateway_timeout"}), missing], "gateway_timeout", 1),
            ([(503, {"code": "unavailable"}), (204, None)], None, 2),
        ]:
            with self.subTest(code=code):
                self.calls.clear()
                self.replies["GET", CAPABILITIES] = [(200, {"capabilities": ["volumes.v1"]})]
                self.replies["DELETE", item] = replies
                if code:
                    with self.assertRaises(Error) as caught:
                        self.client.delete_volume("vol_a")
                    self.assertEqual(caught.exception.code, code)
                else:
                    self.client.delete_volume("vol_a")
                self.assertEqual(self.calls.count(("DELETE", item, None)), count)

    def test_retry_and_open_error_codes(self):
        self.replies["GET", CAPABILITIES] = [(200, {"capabilities": ["volumes.v1"]})] * 4
        self.replies["POST", VOLUMES] = [(503, {"code": "unavailable"})]
        self.replies["GET", VOLUMES] = [(503, {"code": "unavailable"}), (200, {"volumes": []})]
        for code in ("volume_in_use", "future_volume_error"):
            self.replies["DELETE", VOLUMES + "/vol_a"] = [(409, {"code": code})]
            with self.assertRaises(Error) as caught:
                self.client.delete_volume("vol_a")
            self.assertEqual((caught.exception.code, caught.exception.status_code), (code, 409))
        with self.assertRaises(Error) as caught:
            self.client.create_volume(CreateVolumeRequest(size_bytes=SIZE))
        self.assertEqual(caught.exception.code, "unavailable")
        self.assertEqual(self.client.list_volumes(), [])
        self.assertEqual([method for method, path, _ in self.calls if path == VOLUMES], ["POST", "GET", "GET"])
