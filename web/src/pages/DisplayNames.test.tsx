import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { accountDisplay, resolveKeyDisplay, captureReasonLabel, accountingQualityLabel, formatPriceSource } from './RequestsHelpers';
import { RequestsDetailModal } from './RequestsDetailModal';
import { RequestsTable } from './RequestsTable';
import type { Detail, Key, RequestRecord } from '../lib/types';

describe('friendly identities', () => {
  // Display names never replace the identity used by queries or row keys.
  it('uses names and readable fallbacks without exposing account indices', () => {
    expect(accountDisplay('private-a', '生产账户')).toBe('生产账户');
    expect(accountDisplay('private-a', '  生产账户  ')).toBe('生产账户');
    expect(accountDisplay('private-b', '生产账户')).toBe('生产账户');
    expect(accountDisplay('private-a', 'private-a')).toBe('账户名称不可用');
    expect(accountDisplay('private-a')).toBe('账户名称不可用');
    expect(accountDisplay('')).toBe('未关联账户');
  });

  // Unknown IDs and known unnamed Keys have different, honest fallbacks.
  it('preserves Key identity and deletion metadata without displaying the ID', () => {
    const keys = new Map([['private-key', { id: 'private-key', label: '', deleted_at: '2026-01-01' } as Key]]);
    expect(resolveKeyDisplay('private-key', keys)).toEqual({
      label: '未命名 Key', fullId: 'private-key', isFallback: true, isDeleted: true,
    });
    expect(resolveKeyDisplay('unknown-key', keys).label).toBe('Key 名称不可用');
  });

  // Unknown machine codes must not be passed through as user-facing labels.
  it('translates status codes with explicit unknown fallbacks', () => {
    expect(accountingQualityLabel('complete')).toBe('完整');
    expect(accountingQualityLabel('future-code')).toBe('未知记账质量');
    expect(captureReasonLabel('trace_collision')).toBe('追踪关联冲突');
    expect(captureReasonLabel('future-code')).toBe('未知采集原因');
    expect(formatPriceSource('future-code').label).toBe('未知定价来源');
  });

  // A real long account name uses the same label, ellipsis class and full-name
  // tooltip in request rows and account groups; the index stays an identity only.
  it('presents the same account name in request rows and account groups', () => {
    const name = 'codex-60c4e13c-long-account-name@example.test';
    const record = { id: 'private-request', auth_index: 'private-account', account_name: `  ${name}  `,
      time: '2026-10-22T00:00:00Z', tokens: {}, provider: 'codex' } as RequestRecord;
    const props = { onViewChange: () => {}, page: 1, hasMore: false, onPageChange: () => {},
      keysMap: new Map<string, Key>(), onOpenDetail: () => {} };
    const { rerender } = render(<RequestsTable {...props} view="requests" records={[record]} />);
    const requestName = screen.getByTitle(name);
    expect(requestName).toHaveTextContent(name);
    expect(requestName).toHaveClass('requests-account-name', 'requests-subtext');
    expect(screen.queryByText('private-account')).not.toBeInTheDocument();
    rerender(<RequestsTable {...props} view="account" groups={[{
      key: 'private-account', auth_index: 'private-account', label: `  ${name}  `,
      requests: 1, failures: 0, tokens: 0, cost_micro_usd: 0,
    }]} />);
    expect(screen.getByTitle(name)).toHaveTextContent(name);
    expect(screen.getByTitle(name)).toHaveClass('requests-account-name');
    expect(screen.queryByText('private-account')).not.toBeInTheDocument();
  });

  // Native details is closed initially; identifiers remain available for diagnostics.
  it('keeps exact identifiers in collapsed diagnostics', () => {
    const detail = { record: { id: 'private-request', trace_id: 'private-trace', key_id: 'private-key',
      auth_index: 'private-account', time: '2026-10-22T00:00:00Z', accounting_quality: 'complete',
      tokens: {}, model: 'model', provider: 'provider' } } as Detail;
    render(<RequestsDetailModal detail={detail} loading={false} error={null} onClose={() => {}} keysMap={new Map()} />);
    expect(screen.getByRole('heading', { name: '请求详情' })).toBeInTheDocument();
    const diagnostics = screen.getByText('技术信息').closest('details');
    expect(diagnostics).not.toHaveAttribute('open');
    expect(diagnostics).toHaveTextContent('private-account');
    expect(diagnostics).toHaveTextContent('private-request');
    expect(screen.getByText('账户名称不可用')).toBeInTheDocument();
  });
});
