// Test function for the integration tests: the event selects a behavior.
const fs = require('fs');

// A slow init, on demand.
if (process.env.INIT_SLEEP_MS) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, Number(process.env.INIT_SLEEP_MS));
}

exports.handler = async (event, context) => {
  switch (event.op) {
    case 'sleep':
      await new Promise((r) => setTimeout(r, event.ms));
      return { slept: event.ms };
    case 'throw':
      throw new Error('boom from handler');
    case 'exit':
      process.exit(3);
    case 'exit-after-response':
      // Not a timer: the runtime interface client blocks the event loop
      // while it waits for the next invocation, so a timer would only fire
      // then. A detached shell kills the process once it is idle.
      require('child_process').spawn('sh', ['-c', `sleep 0.3; kill -KILL ${process.pid}`],
        { detached: true, stdio: 'ignore' }).unref();
      return { bye: true };
    case 'oom': {
      const keep = [];
      for (;;) keep.push(Buffer.alloc(8 * 1024 * 1024, 1));
    }
    case 'big-response':
      return 'x'.repeat(7 * 1024 * 1024);
    case 'sdk-invoke': {
      // AWS SDK v3, pointed at another runtime with endpoint-url, SigV4
      // with the access key from the environment.
      const { LambdaClient, InvokeCommand } = require('@aws-sdk/client-lambda');
      const client = new LambdaClient({ endpoint: process.env.TARGET_ENDPOINT, region: process.env.AWS_REGION });
      const out = await client.send(new InvokeCommand({
        FunctionName: 'function', Payload: Buffer.from(JSON.stringify({ via: 'sdk' })),
      }));
      return { statusCode: out.StatusCode, functionError: out.FunctionError || null,
               payload: JSON.parse(Buffer.from(out.Payload).toString()) };
    }
    case 'scan-environ': {
      // Looks for credentials in the environment of every process, as
      // malicious function code would, for event.ms milliseconds.
      const deadline = Date.now() + event.ms;
      const found = new Set();
      let reads = 0;
      while (Date.now() < deadline) {
        for (const pid of fs.readdirSync('/proc')) {
          if (!/^\d+$/.test(pid)) continue;
          try {
            const env = fs.readFileSync(`/proc/${pid}/environ`, 'latin1');
            reads++;
            for (const kv of env.split('\0')) {
              if (/^LOCH_RUNTIME_(TOKEN|ACCESS_KEY)=/.test(kv) || (event.marker && kv.includes(event.marker))) {
                found.add(kv.split('=')[0]);
              }
            }
          } catch (e) { /* not readable */ }
        }
        await new Promise((r) => setImmediate(r));
      }
      return { found: [...found], reads };
    }
    case 'environ':
      try {
        fs.readFileSync('/proc/1/environ');
        return { readable: true };
      } catch (e) {
        return { readable: false, code: e.code };
      }
    default:
      return { event, pid: process.pid, remainingMs: context.getRemainingTimeInMillis(),
               runtimeApi: process.env.AWS_LAMBDA_RUNTIME_API, token: process.env.LOCH_RUNTIME_TOKEN || null };
  }
};
