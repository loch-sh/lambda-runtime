# Test function for the integration tests: the event selects a behavior.
import os
import threading
import time


def handler(event, context):
    op = event.get("op")
    if op == "sleep":
        time.sleep(event["ms"] / 1000)
        return {"slept": event["ms"]}
    if op == "throw":
        raise RuntimeError("boom from handler")
    if op == "exit":
        os._exit(3)
    if op == "exit-after-response":
        threading.Timer(0.05, lambda: os._exit(5)).start()
        return {"bye": True}
    if op == "oom":
        keep = []
        while True:
            keep.append(bytearray(8 * 1024 * 1024))
    if op == "environ":
        try:
            open("/proc/1/environ", "rb").read()
            return {"readable": True}
        except OSError as e:
            return {"readable": False, "code": e.errno}
    return {"event": event, "pid": os.getpid(), "remainingMs": context.get_remaining_time_in_millis(),
            "runtimeApi": os.environ.get("AWS_LAMBDA_RUNTIME_API"), "token": os.environ.get("LOCH_RUNTIME_TOKEN")}
