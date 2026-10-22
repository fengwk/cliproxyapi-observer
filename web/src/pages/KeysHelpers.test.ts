import { describe, it, expect, vi } from 'vitest';
import {
  validateCustomSecret,
  validateConcurrency,
  parseQuotaLimit,
  validateExpiry,
  copyToClipboard,
} from './KeysHelpers';

describe('KeysHelpers Unit Tests', () => {
  describe('validateCustomSecret', () => {
    // 意图：测试合法 8..256 可见 ASCII 字符通过
    it('accepts valid 8..256 ASCII strings', () => {
      expect(validateCustomSecret('12345678').valid).toBe(true);
      expect(validateCustomSecret('A!b@C#d$E%f^1234567890').valid).toBe(true);
      expect(validateCustomSecret('a'.repeat(256)).valid).toBe(true);
    });

    // 意图：测试包含空格或换行等非法字符被拒绝
    it('rejects secrets containing whitespace or newlines without trimming', () => {
      expect(validateCustomSecret('  12345678').valid).toBe(false);
      expect(validateCustomSecret('12345678  ').valid).toBe(false);
      expect(validateCustomSecret('1234 5678').valid).toBe(false);
      expect(validateCustomSecret('12345678\n').valid).toBe(false);
      expect(validateCustomSecret('12345678\t').valid).toBe(false);
    });

    // 意图：测试非 ASCII 字符被拒绝
    it('rejects non-ASCII characters', () => {
      expect(validateCustomSecret('密钥12345678').valid).toBe(false);
      expect(validateCustomSecret('secret🚀123').valid).toBe(false);
    });

    // 意图：测试长度小于 8 或大于 256 被拒绝
    it('rejects length < 8 or > 256', () => {
      expect(validateCustomSecret('short').valid).toBe(false);
      expect(validateCustomSecret('a'.repeat(257)).valid).toBe(false);
    });
  });

  describe('validateConcurrency', () => {
    // 意图：测试 1..1000000 范围内的合法整数通过
    it('accepts valid integers between 1 and 1000000', () => {
      expect(validateConcurrency('1')).toEqual({ valid: true, value: 1 });
      expect(validateConcurrency('5')).toEqual({ valid: true, value: 5 });
      expect(validateConcurrency('1000000')).toEqual({ valid: true, value: 1000000 });
      expect(validateConcurrency('9000000000000000')).toEqual({ valid: true, value: 9000000000000000 });
    });

    // 意图：测试小数被拒绝（严禁隐式 parseInt 截断）
    it('rejects decimal inputs such as 2.5', () => {
      const res = validateConcurrency('2.5');
      expect(res.valid).toBe(false);
      expect(res.error).toBe('最大并发数必须为正整数，且不超过 9000000000000000');
    });

    // 与后端的安全整数边界一致，避免破坏现有大并发配置的无关编辑。
    it('rejects zero, negative, overflow, and non-numeric strings', () => {
      expect(validateConcurrency('0').valid).toBe(false);
      expect(validateConcurrency('-1').valid).toBe(false);
      expect(validateConcurrency('9000000000000001').valid).toBe(false);
      expect(validateConcurrency('abc').valid).toBe(false);
      expect(validateConcurrency('').valid).toBe(false);
    });
  });

  describe('parseQuotaLimit', () => {
    // 意图：测试未开启限额时返回 0
    it('returns 0 when quota is not enabled', () => {
      expect(parseQuotaLimit(false, '', '5小时额度')).toEqual({ valid: true, value: 0 });
    });

    // 意图：测试开启限额但输入为空时必须报错拒绝
    it('rejects empty input when quota is enabled', () => {
      const res = parseQuotaLimit(true, '', '5小时额度');
      expect(res.valid).toBe(false);
      expect(res.error).toContain('已开启，金额不能为空且必须大于 0');
    });

    // 意图：测试开启限额但输入为 0 或 0.00 时必须报错拒绝
    it('rejects 0 or 0.00 when quota is enabled', () => {
      const res1 = parseQuotaLimit(true, '0', '5小时额度');
      expect(res1.valid).toBe(false);
      expect(res1.error).toContain('已开启，金额必须大于 0');

      const res2 = parseQuotaLimit(true, '0.00', '5小时额度');
      expect(res2.valid).toBe(false);
      expect(res2.error).toContain('已开启，金额必须大于 0');
    });

    // 意图：测试合法金额解析为 micro-USD
    it('parses valid positive USD string to micro-USD', () => {
      expect(parseQuotaLimit(true, '5', '5小时额度')).toEqual({ valid: true, value: 5000000 });
      expect(parseQuotaLimit(true, '1.5', '5小时额度')).toEqual({ valid: true, value: 1500000 });
    });
  });

  describe('validateExpiry', () => {
    // 意图：测试未开启有效期时返回 null
    it('returns null when expiry is not enabled', () => {
      expect(validateExpiry(false, '')).toEqual({ valid: true, iso: null });
    });

    // 意图：测试开启有效期但未提供时间时拒绝
    it('rejects empty input when expiry is enabled', () => {
      const res = validateExpiry(true, '');
      expect(res.valid).toBe(false);
      expect(res.error).toBe('开启有效期后必须指定到期时间');
    });

    // 意图：测试编辑时未修改时间保留原始 RFC3339 纳秒和时区
    it('preserves original RFC3339 nanoseconds and timezone when unchanged', () => {
      const origISO = '2026-10-19T14:30:00.123456789Z';
      const d = new Date(origISO);
      const localInput = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}T${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
      const res = validateExpiry(true, localInput, origISO);
      expect(res.valid).toBe(true);
      expect(res.iso).toBe(origISO);
    });

    // 意图：同一分钟后的垃圾串不能冒充未修改输入
    it('rejects pseudo-input that only shares the original minute', () => {
      const origISO = '2026-10-19T14:30:00.123456789Z';
      const d = new Date(origISO);
      const localInput = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}T${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
      const res = validateExpiry(true, `${localInput}junk`, origISO);
      expect(res.valid).toBe(false);
      expect(res.iso).toBeNull();
    });

    // 意图：明确修改秒数时走正常解析，而不是保留原始纳秒
    it('converts an explicit seconds edit instead of preserving the original', () => {
      const origISO = '2026-10-19T14:30:00.123456789Z';
      const d = new Date(origISO);
      const localInput = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}T${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
      const res = validateExpiry(true, `${localInput}:30`, origISO);
      expect(res.valid).toBe(true);
      expect(res.iso).not.toBe(origISO);
      expect(new Date(res.iso as string).getSeconds()).toBe(30);
    });
  });

  describe('copyToClipboard', () => {
    // 意图：测试剪贴板成功与失败路径
    it('returns true on success and false on rejection', async () => {
      const mockWrite = vi.fn().mockResolvedValue(undefined);
      Object.assign(navigator, { clipboard: { writeText: mockWrite } });

      const ok = await copyToClipboard('test-copy');
      expect(ok).toBe(true);
      expect(mockWrite).toHaveBeenCalledWith('test-copy');

      mockWrite.mockRejectedValueOnce(new Error('fail'));
      const fail = await copyToClipboard('test-copy');
      expect(fail).toBe(false);
    });
  });
});
