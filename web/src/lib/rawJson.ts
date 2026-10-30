/**
 * Exact JSON number preservation for requestBody condition values.
 *
 * Modern browsers expose JSON.parse reviver `context.source` and JSON.rawJSON.
 * Observer requires those capabilities whenever a numeric requestBody equals
 * value is loaded or saved. There is no rounded fallback.
 */
export interface JSONNumberSupport {
  raw: boolean;
  source: boolean;
}

interface ReviverContext {
  source?: string;
}

type NativeJSON = typeof JSON & {
  rawJSON?: (text: string) => unknown;
  parse: (
    text: string,
    reviver: (this: unknown, key: string, value: unknown, context?: ReviverContext) => unknown,
  ) => unknown;
};

const jsonNumberToken = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/;

export function detectJSONNumberSupport(): JSONNumberSupport {
  const native = JSON as NativeJSON;
  let source = false;
  try {
    native.parse('0', function (_key: string, value: unknown, context?: ReviverContext) {
      source = context !== undefined && typeof context.source === 'string';
      return value;
    });
  } catch {
    source = false;
  }
  return { raw: typeof native.rawJSON === 'function', source };
}

export function isJSONNumberToken(text: string): boolean {
  return jsonNumberToken.test(text);
}

/**
 * An exact numeric scalar. `toJSON` returns the native JSON.rawJSON object so
 * JSON.stringify emits the original token and no custom wrapper can inject a
 * value that was not validated here.
 */
export class RawJSONNumber {
  readonly text: string;
  private readonly raw: unknown;

  constructor(text: string, support: JSONNumberSupport = detectJSONNumberSupport()) {
    if (!isJSONNumberToken(text)) throw new Error('请求体数字不是合法 JSON 数字');
    if (!support.raw) {
      throw new Error('当前浏览器不支持 JSON.rawJSON，无法安全保存精确数字条件');
    }
    this.text = text;
    this.raw = (JSON as NativeJSON).rawJSON!(text);
  }

  toJSON(): unknown {
    return this.raw;
  }
}

export function isRawJSONNumber(value: unknown): value is RawJSONNumber {
  return value instanceof RawJSONNumber;
}

/**
 * Parse settings while preserving exact requestBody.equals number tokens.
 * Without reviver source support, loading any numeric condition is rejected
 * instead of silently rounding it through JavaScript Number.
 */
export function parseSettingsJSON(
  text: string,
  support: JSONNumberSupport = detectJSONNumberSupport(),
): unknown {
  const parsed = (JSON as NativeJSON).parse(text, function (_key: string, value: unknown, context?: ReviverContext) {
    if (_key !== 'equals' || typeof value !== 'number') return value;
    if (!support.source || !context || typeof context.source !== 'string') {
      throw new Error('当前浏览器缺少 JSON.parse reviver context.source，无法安全加载精确数字条件');
    }
    if (!isJSONNumberToken(context.source)) {
      throw new Error('服务端返回了非法 JSON 数字条件');
    }
    return new RawJSONNumber(context.source, support);
  });
  validateExactNumbers(parsed);
  return parsed;
}

/**
 * JSON.stringify already uses RawJSONNumber.toJSON. This wrapper makes the
 * exact-number contract explicit and rejects an impossible undefined result.
 */
export function stringifyWithRawJSON(value: unknown): string {
  const out = JSON.stringify(value);
  if (out === undefined) throw new Error('无法序列化设置');
  return out;
}

/**
 * A raw token is valid only as the equals value of a requestBody leaf. Numeric
 * equals values are marked before their sibling path may be visited by the
 * bottom-up reviver, so this post-parse pass validates the final shape.
 */
function validateExactNumbers(value: unknown): void {
  if (Array.isArray(value)) {
    for (const item of value) validateExactNumbers(item);
    return;
  }
  if (!value || typeof value !== 'object' || isRawJSONNumber(value)) return;
  const record = value as Record<string, unknown>;
  for (const [key, child] of Object.entries(record)) {
    if (isRawJSONNumber(child)) {
      const keys = Object.keys(record);
      if (key === 'equals' && typeof record.path === 'string' &&
          keys.every((field) => field === 'path' || field === 'equals')) {
        continue;
      }
      throw new Error('服务端返回了未绑定的精确数字条件');
    }
    validateExactNumbers(child);
  }
}
