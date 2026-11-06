// Test function for the integration tests: the event selects a behavior.
const fs = require('fs');

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
      setTimeout(() => process.exit(5), 50);
      return { bye: true };
    case 'oom': {
      const keep = [];
      for (;;) keep.push(Buffer.alloc(8 * 1024 * 1024, 1));
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
