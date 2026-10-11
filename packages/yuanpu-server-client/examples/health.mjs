import { createServerClient, ServerApiError } from '../dist/index.mjs';

const client = createServerClient({ baseUrl: process.env.YUANPU_SERVER_URL ?? 'http://127.0.0.1:8788' });
console.log('health:', await client.health());
try {
  console.log('ready:', await client.ready());
} catch (error) {
  if (!(error instanceof ServerApiError)) throw error;
  console.error('ready:', { code: error.code, message: error.message, requestId: error.requestId });
  process.exitCode = 1;
}
