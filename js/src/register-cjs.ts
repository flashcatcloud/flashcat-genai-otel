/**
 * CommonJS zero-code entry point: `node --require @flashcat/otel/register app.js`.
 * CommonJS apps do not need the OpenInference ESM fallback, which requires top-level await.
 */

import { boot } from './register-common';

boot();
