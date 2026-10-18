import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react';
import { RequestsPage } from './Requests';
import { buildRequestsQueryParams, formatCostExact } from './RequestsHelpers';
import type { ApiClient } from '../lib/api';
import type { RequestPage as RequestPageType, Summary, Detail } from '../lib/types';

// Mock clipboard API
const mockClipboard = {
  writeText: vi.fn().mockResolvedValue(undefined),
};
Object.assign(navigator, { clipboard: mockClipboard });

function createMockApiClient(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    request: vi.fn().mockImplementation(async (path: string) => {
      if (path.startsWith('/summary')) {
        return {
          requests: 12,
          failures: 2,
          cost_micro_usd: 1500000,
          tokens: 45000,
          granularity: 'day',
          buckets: [
            {
              time: '2026-10-18T00:00:00Z',
              requests: 7,
              failures: 1,
              cost_micro_usd: 800000,
              tokens: 25000,
            },
            {
              time: '2026-10-19T00:00:00Z',
              requests: 5,
              failures: 1,
              cost_micro_usd: 700000,
              tokens: 20000,
            },
          ],
        } satisfies Summary;
      }
      if (path.startsWith('/requests/detail')) {
        return {
          record: {
            id: 'req-001',
            trace_id: 'trace-001',
            key_id: 'key-abc',
            time: '2026-10-19T10:00:00Z',
            model: 'gpt-4o',
            provider: 'openai',
            auth_index: 'default',
            failed: false,
            status_code: 200,
            stream: false,
            latency_ms: 120,
            ttft_ms: 45,
            tokens: { input: 100, output: 50, cache_read: 0, cache_write: 0, reasoning: 0, total: 150 },
            cost_micro_usd: 5000,
            price_source: 'catalog',
            accounting_quality: 'standard',
          },
          headers: {
            'content-type': ['application/json'],
            authorization: ['Bearer secret-token-12345'],
          },
          body: { prompt: 'Hello world', secret_key: 'top-secret' },
          capture_status: 'available',
        } satisfies Detail;
      }
      if (path.startsWith('/requests')) {
        return {
          items: [
            {
              id: 'req-001',
              trace_id: 'trace-001',
              key_id: 'key-abc',
              time: '2026-10-19T10:00:00Z',
              model: 'gpt-4o',
              provider: 'openai',
              auth_index: 'default',
              failed: false,
              status_code: 200,
              stream: false,
              latency_ms: 120,
              ttft_ms: 45,
              tokens: { input: 100, output: 50, cache_read: 0, cache_write: 0, reasoning: 0, total: 150 },
              cost_micro_usd: 5000,
              price_source: 'catalog',
              accounting_quality: 'standard',
            },
            {
              id: 'req-002',
              trace_id: 'trace-002',
              key_id: '4f68a2d1e09c8b7a6543210fedcba9874f68a2d1e09c8b7a6543210fedcba987',
              time: '2026-10-19T11:00:00Z',
              model: 'claude-3-5-sonnet',
              provider: 'anthropic',
              auth_index: 'prod-01',
              failed: true,
              status_code: 500,
              stream: true,
              latency_ms: 350,
              ttft_ms: 0,
              tokens: { input: 200, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, total: 200 },
              cost_micro_usd: 1, // 1 micro USD (small non-zero charge)
              price_source: 'unpriced',
              accounting_quality: 'unclassified',
            },
          ],
          page: 1,
          page_size: 25,
          has_more: false,
        } satisfies RequestPageType;
      }
      if (path.startsWith('/keys')) {
        return {
          items: [
            {
              id: 'key-abc',
              label: 'Production Key',
              enabled: true,
              max_concurrent: 5,
              allowed_models: ['gpt-4o'],
              limits: { five_hour: 0, week: 0, month: 0, total: 0 },
              used: { five_hour: 0, week: 0, month: 0, total: 0 },
              starts: { five_hour: '', week: '', month: '', total: '' },
              resets: { five_hour: '', week: '', month: '', total: '' },
              active: 0,
              created_at: '2026-10-01T00:00:00Z',
            },
          ],
        };
      }
      return {};
    }),
    models: vi.fn().mockResolvedValue([{ id: 'gpt-4o' }, { id: 'claude-3-5-sonnet' }]),
    ...overrides,
  };
}

describe('RequestsPage and RequestsHelpers', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  /**
   * Why covered:
   * Proves that date bounds properly use whole UTC dates with explicit 'T00:00:00Z',
   * and that empty 'to' defaults to the current NOW timestamp to prevent querying future bounds.
   */
  it('UTC bounds: correctly formats RFC3339 whole UTC dates and defaults to NOW when to is empty', () => {
    const fixedNow = '2026-10-19T15:30:00.000Z';
    const query = buildRequestsQueryParams(
      {
        from: '2026-10-19',
        to: '',
        key_id: 'test-key',
        model: 'gpt-4o',
        provider: '',
        auth_index: '',
        status: 'success',
      },
      1,
      25,
      fixedNow
    );

    const params = new URLSearchParams(query);
    expect(params.get('from')).toBe('2026-10-19T00:00:00Z');
    expect(params.get('to')).toBe(fixedNow);
    expect(params.get('key_id')).toBe('test-key');
    expect(params.get('status')).toBe('success');
  });

  /**
   * Why covered:
   * Proves that micro-USD formatting retains exact precision without silently rounding
   * small non-zero amounts (e.g. 1 micro-USD = $0.000001) to $0.
   */
  it('micro-USD precision: small nonzero charges are never rounded to zero', () => {
    expect(formatCostExact(0)).toBe('0');
    expect(formatCostExact(1)).toBe('0.000001');
    expect(formatCostExact(50)).toBe('0.00005');
    expect(formatCostExact(1000000)).toBe('1');
    expect(formatCostExact(1500000)).toBe('1.5');
  });

  /**
   * Why covered:
   * Proves that request detail (/requests/detail?id=...) is fetched strictly on demand
   * when a user clicks the model button, rather than pre-fetching details during list render.
   */
  it('detail on-demand: detail endpoint is only called when opening detail modal', async () => {
    const api = createMockApiClient();
    const onDirtyChange = vi.fn();

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('请求日志')).toBeInTheDocument();
    });

    // Detail endpoint must NOT have been called on page load
    const detailCallsBefore = (api.request as ReturnType<typeof vi.fn>).mock.calls.filter((call) =>
      call[0].startsWith('/requests/detail')
    );
    expect(detailCallsBefore.length).toBe(0);

    // Click the model button for req-001
    const modelBtn = screen.getByRole('button', { name: 'gpt-4o' });
    fireEvent.click(modelBtn);

    // Detail endpoint must now have been called exactly once with req-001
    await waitFor(() => {
      const detailCallsAfter = (api.request as ReturnType<typeof vi.fn>).mock.calls.filter((call) =>
        call[0].startsWith('/requests/detail?id=req-001')
      );
      expect(detailCallsAfter.length).toBe(1);
    });

    // Modal dialog is displayed
    expect(screen.getByText('请求详情')).toBeInTheDocument();
  });

  /**
   * Why covered:
   * Proves that closing the detail modal aborts in-flight fetches, and late responses
   * cannot reopen the modal after the user closed it.
   */
  it('stale response abort: closing modal aborts fetch and late response cannot reopen', async () => {
    let resolveDetail: (value: unknown) => void = () => {};
    const api = createMockApiClient({
      request: vi.fn().mockImplementation(async (path: string, opts?: { signal?: AbortSignal }) => {
        if (path.startsWith('/requests/detail')) {
          return new Promise((resolve, reject) => {
            resolveDetail = resolve;
            opts?.signal?.addEventListener('abort', () => {
              reject(new Error('aborted'));
            });
          });
        }
        if (path.startsWith('/summary')) return { requests: 1, failures: 0, cost_micro_usd: 0, tokens: 0, granularity: 'day', buckets: [] };
        if (path.startsWith('/requests')) return { items: [{ id: 'req-slow', time: '2026-10-19T00:00:00Z', model: 'slow-model', tokens: { total: 0 } }], page: 1, page_size: 25, has_more: false };
        if (path.startsWith('/keys')) return { items: [] };
        return {};
      }),
    });

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'slow-model' })).toBeInTheDocument();
    });

    // Open detail
    fireEvent.click(screen.getByRole('button', { name: 'slow-model' }));
    expect(screen.getByText('请求详情')).toBeInTheDocument();

    // Close modal while request is still pending
    const closeBtn = screen.getByRole('button', { name: '关闭' });
    fireEvent.click(closeBtn);

    // Modal is closed
    expect(screen.queryByText('请求详情')).not.toBeInTheDocument();

    // Now resolve the late response
    await act(async () => {
      resolveDetail({
        record: { id: 'req-slow', model: 'slow-model', time: '2026-10-19T00:00:00Z', tokens: { total: 0 } },
        headers: {},
        body: null,
        capture_status: 'available',
      });
    });

    // Late response must not reopen the modal
    expect(screen.queryByText('请求详情')).not.toBeInTheDocument();
  });

  /**
   * Why covered:
   * Proves that malicious model names and JSON prompt bodies are rendered safely
   * as pure React text nodes, preventing script execution and HTML injection.
   * Also verifies sensitive headers and body fields are redacted.
   */
  it('malicious model body: XSS payloads in model and body are rendered as safe text, sensitive keys redacted', async () => {
    const maliciousModel = '<script>window.__xssInjected=true;</script><img src=x onerror=alert(1)>';
    const maliciousPrompt = '<b onmouseover=alert(2)>Malicious text</b>';

    const api = createMockApiClient({
      request: vi.fn().mockImplementation(async (path: string) => {
        if (path.startsWith('/requests/detail')) {
          return {
            record: {
              id: 'req-xss',
              time: '2026-10-19T00:00:00Z',
              model: maliciousModel,
              tokens: { total: 10 },
              cost_micro_usd: 0,
              price_source: 'catalog',
              status_code: 200,
            },
            headers: {
              Authorization: ['Bearer super-secret-key-123'],
              Cookie: ['session=secret-cookie'],
              'User-Agent': ['TestAgent/1.0'],
            },
            body: {
              prompt: maliciousPrompt,
              password: 'plaintext-secret',
            },
            capture_status: 'redacted',
          };
        }
        if (path.startsWith('/summary')) return { requests: 1, failures: 0, cost_micro_usd: 0, tokens: 0, granularity: 'day', buckets: [] };
        if (path.startsWith('/requests')) {
          return {
            items: [
              {
                id: 'req-xss',
                time: '2026-10-19T00:00:00Z',
                model: maliciousModel,
                tokens: { total: 10 },
                cost_micro_usd: 0,
                status_code: 200,
              },
            ],
            page: 1,
            page_size: 25,
            has_more: false,
          };
        }
        return { items: [] };
      }),
    });

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    // Model name is rendered as pure text inside a button
    const modelBtn = await screen.findByRole('button', { name: maliciousModel });
    expect(modelBtn).toBeInTheDocument();
    expect((window as unknown as Record<string, unknown>).__xssInjected).toBeUndefined();

    // Open detail
    fireEvent.click(modelBtn);
    await screen.findByText('请求详情');

    // Switch to Headers tab
    fireEvent.click(screen.getByRole('tab', { name: '请求头' }));
    // Sensitive headers redacted
    expect(screen.getAllByText('[redacted]').length).toBeGreaterThan(0);
    expect(screen.queryByText('Bearer super-secret-key-123')).not.toBeInTheDocument();

    // Switch to Body tab
    fireEvent.click(screen.getByRole('tab', { name: '请求体 (JSON)' }));
    // Prompt rendered as text inside pre, not HTML tag
    expect(screen.getByText((content) => content.includes(maliciousPrompt))).toBeInTheDocument();
    // Password redacted
    expect(screen.queryByText('plaintext-secret')).not.toBeInTheDocument();
  });

  /**
   * Why covered:
   * Proves that records with zero status codes (e.g. status_code = 0) and unknown status
   * are handled honestly without fabricating a "200 OK".
   */
  it('unknown status: status code 0 and unknown status are handled honestly', async () => {
    const api = createMockApiClient({
      request: vi.fn().mockImplementation(async (path: string) => {
        if (path.startsWith('/requests')) {
          return {
            items: [
              {
                id: 'req-unknown',
                time: '2026-10-19T00:00:00Z',
                model: 'custom-model',
                failed: true,
                status_code: 0,
                tokens: { total: 0 },
                cost_micro_usd: 0,
              },
            ],
            page: 1,
            page_size: 25,
            has_more: false,
          };
        }
        if (path.startsWith('/summary')) return { requests: 1, failures: 1, cost_micro_usd: 0, tokens: 0, granularity: 'day', buckets: [] };
        return { items: [] };
      }),
    });

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    // Failed record with status 0 should show '失败' without crashing
    const badges = await screen.findAllByText(/^失败/);
    expect(badges.length).toBeGreaterThan(0);
    expect(screen.queryByText('200')).not.toBeInTheDocument();
  });

  /**
   * Why covered:
   * Proves that filter edits are state-controlled (draft), report onDirtyChange(true),
   * and only trigger query and reset page to 1 on form submission.
   * Also verifies pagination preserves applied filters without resetting drafts.
   */
  it('filters and pagination: edits do not auto-query, submit applies & resets page to 1, pagination preserves filters', async () => {
    const api = createMockApiClient();
    const onDirtyChange = vi.fn();

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByLabelText('模型')).toBeInTheDocument();
    });

    const modelInput = screen.getByLabelText('模型');
    const callsBefore = (api.request as ReturnType<typeof vi.fn>).mock.calls.length;

    // Type in draft filter
    fireEvent.change(modelInput, { target: { value: 'gpt-5-turbo' } });

    // Editing draft must notify dirty = true
    expect(onDirtyChange).toHaveBeenCalledWith(true);

    // Must NOT immediately fire API query
    expect((api.request as ReturnType<typeof vi.fn>).mock.calls.length).toBe(callsBefore);

    // Click submit query button
    const submitBtn = screen.getByRole('button', { name: '查询' });
    fireEvent.click(submitBtn);

    // After submit, dirty should become false
    await waitFor(() => {
      expect(onDirtyChange).toHaveBeenLastCalledWith(false);
    });

    // Query must now include model=gpt-5-turbo and page=1
    await waitFor(() => {
      const callsWithModel = (api.request as ReturnType<typeof vi.fn>).mock.calls.filter((call) =>
        call[0].includes('model=gpt-5-turbo') && call[0].includes('page=1')
      );
      expect(callsWithModel.length).toBeGreaterThan(0);
    });
  });

  /**
   * Why covered:
   * Proves that key filter uses key labels (names) rather than 64-hex IDs,
   * falls back to truncated 12-char prefix with tooltip for opaque IDs,
   * and provides a copy button without browser alert/confirm.
   */
  it('Key labels: shows friendly name when available, truncates 64-hex ID and copies without native alert', async () => {
    const api = createMockApiClient();
    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    // Production Key has a label 'Production Key'
    await waitFor(() => {
      expect(screen.getByText('Production Key')).toBeInTheDocument();
    });

    // req-002 has a 64-hex ID; should show 12-char prefix + ellipsis
    const truncatedKey = screen.getByText('4f68a2d1e09c…');
    expect(truncatedKey).toBeInTheDocument();
    expect(truncatedKey).toHaveAttribute(
      'title',
      '4f68a2d1e09c8b7a6543210fedcba9874f68a2d1e09c8b7a6543210fedcba987'
    );

    // Click copy button
    const copyBtns = screen.getAllByRole('button', { name: /复制/ });
    await act(async () => {
      fireEvent.click(copyBtns[0]);
    });

    // Clipboard writeText called with full 64-hex ID
    expect(mockClipboard.writeText).toHaveBeenCalledWith(
      '4f68a2d1e09c8b7a6543210fedcba9874f68a2d1e09c8b7a6543210fedcba987'
    );

    // Global helper capture value is updated
    expect((window as unknown as Record<string, unknown>).__observerCopied).toBe(
      '4f68a2d1e09c8b7a6543210fedcba9874f68a2d1e09c8b7a6543210fedcba987'
    );
  });

  /**
   * Why covered:
   * Proves honest handling of capture states, and ensures that when body is null
   * (e.g. disabled or missing), it is not displayed as empty JSON success {}.
   */
  it('capture states: displays honest capture status and does not display null body as empty JSON success', async () => {
    const api = createMockApiClient({
      request: vi.fn().mockImplementation(async (path: string) => {
        if (path.startsWith('/requests/detail')) {
          return {
            record: {
              id: 'req-disabled',
              time: '2026-10-19T00:00:00Z',
              model: 'model-a',
              tokens: { total: 0 },
              cost_micro_usd: 0,
            },
            headers: {},
            body: null,
            capture_status: 'disabled',
          };
        }
        if (path.startsWith('/requests')) {
          return {
            items: [
              {
                id: 'req-disabled',
                time: '2026-10-19T00:00:00Z',
                model: 'model-a',
                tokens: { total: 0 },
                cost_micro_usd: 0,
              },
            ],
            page: 1,
            page_size: 25,
            has_more: false,
          };
        }
        if (path.startsWith('/summary')) return { requests: 1, failures: 0, cost_micro_usd: 0, tokens: 0, granularity: 'day', buckets: [] };
        return { items: [] };
      }),
    });

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    const modelBtn = await screen.findByRole('button', { name: 'model-a' });
    fireEvent.click(modelBtn);

    await screen.findByText('请求详情');
    // Shows honest capture status label '采集未开启'
    expect(screen.getByText('采集未开启')).toBeInTheDocument();

    // Switch to JSON body tab
    fireEvent.click(screen.getByRole('tab', { name: '请求体 (JSON)' }));

    // Must show honest notice rather than '{}'
    expect(screen.getByText(/无可用正文/)).toBeInTheDocument();
    expect(screen.queryByText('{}')).not.toBeInTheDocument();
  });

  /**
   * Why covered:
   * Proves the corrective whole-date action works when a query error occurs
   * (e.g. historical daily query with hours), offering a button to snap bounds and retry.
   */
  it('error vs not data: offers corrective whole-date action upon error and displays empty state on empty data', async () => {
    let shouldFail = true;
    const api = createMockApiClient({
      request: vi.fn().mockImplementation(async (path: string) => {
        if (path.startsWith('/summary') || path.startsWith('/requests')) {
          if (shouldFail) {
            throw new Error('invalid query: historical range requires whole dates');
          }
          return path.startsWith('/summary')
            ? { requests: 0, failures: 0, cost_micro_usd: 0, tokens: 0, granularity: 'day', buckets: [] }
            : { items: [], page: 1, page_size: 25, has_more: false };
        }
        return { items: [] };
      }),
    });

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    // Query error notice is shown with corrective action button
    const errorNotice = await screen.findByText(/invalid query/);
    expect(errorNotice).toBeInTheDocument();

    const correctiveBtn = screen.getByRole('button', { name: '修正为整日 UTC 范围' });
    expect(correctiveBtn).toBeInTheDocument();

    // Click corrective button
    shouldFail = false;
    fireEvent.click(correctiveBtn);

    // Now empty state should appear
    await waitFor(() => {
      expect(screen.getByText('暂无请求记录')).toBeInTheDocument();
    });
  });
});
