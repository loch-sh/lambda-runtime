// Response streaming handler (awslambda.streamifyResponse): buffered by
// the runtime, delivered whole to the caller.
exports.handler = awslambda.streamifyResponse(async (event, responseStream) => {
  if (event.fail) {
    responseStream.write('partial');
    throw new Error('stream failed');
  }
  responseStream.setContentType('text/plain');
  for (const chunk of ['one ', 'two ', 'three']) {
    responseStream.write(chunk);
    await new Promise((r) => setTimeout(r, 10));
  }
  responseStream.end();
});
