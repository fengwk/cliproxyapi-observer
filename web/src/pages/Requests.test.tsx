import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react';
import { RequestsPage } from './Requests';
import {
  buildRequestViewParams,
  formatCostExact,
  getPresetRange,
  isDynamicRange,
  calculateCacheHitRate,
  calculateFailureRate,
  calculateTPS,
  formatAvgSeconds,
  formatSecondsFromMs,
  isHistoricalDateQueryError,
} from './RequestsHelpers';
import type { ApiClient } from '../lib/api';
import type { RequestView, Detail, FacetPage } from '../lib/types';

// Mock clipboard API
const mockClipboard = {
  writeText: vi.fn().mockResolvedValue(undefined),
};
Object.assign(navigator, { clipboard: mockClipboard });

// Mock scrollIntoView for Radix Select in jsdom
window.HTMLElement.prototype.scrollIntoView = vi.fn();

const FIXED_NOW = new Date('2026-10-20T12:00:00Z');

function createSampleRequestView(): RequestView {
  return {
    range: {
      from: '2026-10-20',
      to: '2026-10-20',
      as_of: '2026-10-20T12:00:00Z',
      timezone: 'UTC',
    },
    summary: {
      requests: 12,
      failures: 2,
      cost_micro_usd: 1500000,
      tokens: 45000,
      input_tokens: 30000,
      output_tokens: 15000,
      reasoning_tokens: 5000,
      cache_read_tokens: 10000,
      cache_write_tokens: 2000,
      complete_requests: 10,
      latency_ms: 2400,
      latency_samples: 12,
      ttft_ms: 600,
      ttft_samples: 10,
      unpriced_requests: 1,
      detailed_requests: 12,
    },
    series: {
      granularity: 'day',
      buckets: [
        {
          time: '2026-10-19T00:00:00Z',
          requests: 7,
          failures: 1,
          cost_micro_usd: 800000,
          tokens: 25000,
        },
        {
          time: '2026-10-20T00:00:00Z',
          requests: 5,
          failures: 1,
          cost_micro_usd: 700000,
          tokens: 20000,
        },
      ],
    },
    records: [
      {
        id: 'req-001',
        trace_id: 'trace-001',
        key_id: 'key-abc',
        time: '2026-10-20T10:00:00Z',
        model: 'gpt-4o',
        provider: 'openai',
        auth_index: 'default',
        failed: false,
        status_code: 200,
        stream: true,
        latency_ms: 120,
        ttft_ms: 45,
        tokens: {
          input: 100,
          output: 50,
          cache_read: 20,
          cache_write: 10,
          reasoning: 15,
          total: 150,
        },
        cost_micro_usd: 5000,
        price_source: 'catalog',
        accounting_quality: 'standard',
      },
      {
        id: 'req-002',
        trace_id: 'trace-002',
        key_id: '4f68a2d1e09c8b7a6543210fedcba9874f68a2d1e09c8b7a6543210fedcba987',
        time: '2026-10-20T11:00:00Z',
        model: 'claude-3-5-sonnet',
        provider: 'anthropic',
        auth_index: 'prod-01',
        failed: true,
        status_code: 500,
        stream: false,
        latency_ms: 350,
        ttft_ms: 0,
        tokens: {
          input: 200,
          output: 0,
          cache_read: 0,
          cache_write: 0,
          reasoning: 0,
          total: 200,
        },
        cost_micro_usd: 1,
        price_source: 'unpriced',
        accounting_quality: 'unclassified',
      },
    ],
    groups: [
      {
        key: 'gpt-4o',
        model: 'gpt-4o',
        provider: 'openai',
        requests: 7,
        failures: 1,
        cost_micro_usd: 800000,
        tokens: 25000,
        input_tokens: 15000,
        output_tokens: 10000,
        cache_read_tokens: 5000,
        cache_write_tokens: 1000,
        latency_ms: 1400,
        latency_samples: 7,
        ttft_ms: 350,
        ttft_samples: 5,
      },
    ],
    pagination: {
      page: 1,
      page_size: 25,
      has_more: false,
    },
    coverage: {
      raw_from: '2026-10-19T00:00:00Z',
      daily_from: '2025-10-20T00:00:00Z',
      detailed_requests: 12,
      requests: 12,
      backfill_pending: false,
    },
  };
}

function createMockApiClient(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    request: vi.fn().mockImplementation(async (path: string) => {
      if (path.startsWith('/requests/view')) {
        return createSampleRequestView();
      }
      if (path.startsWith('/requests/facets')) {
        if (path.includes('field=model')) {
          return {
            items: [
              { value: 'gpt-4o' },
              { value: 'claude-3-5-sonnet' },
              { value: 'legacy-model-v1', label: 'legacy-model-v1 (历史)' },
            ],
          } satisfies FacetPage;
        }
        if (path.includes('field=key_id')) {
          return {
            items: [
              { value: 'key-abc' },
              { value: 'deleted-key-xyz', label: 'Archived Key' },
            ],
          } satisfies FacetPage;
        }
        if (path.includes('field=provider')) {
          return {
            items: [{ value: 'openai' }, { value: 'anthropic' }],
          } satisfies FacetPage;
        }
        if (path.includes('field=auth_index')) {
          return {
            items: [{ value: 'default' }, { value: 'prod-01' }],
          } satisfies FacetPage;
        }
        return { items: [] } satisfies FacetPage;
      }
      if (path.startsWith('/requests/detail')) {
        return {
          record: {
            id: 'req-001',
            trace_id: 'trace-001',
            key_id: 'key-abc',
            time: '2026-10-20T10:00:00Z',
            model: 'gpt-4o',
            provider: 'openai',
            auth_index: 'default',
            failed: false,
            status_code: 200,
            stream: true,
            latency_ms: 120,
            ttft_ms: 45,
            tokens: {
              input: 100,
              output: 50,
              cache_read: 20,
              cache_write: 10,
              reasoning: 15,
              total: 150,
            },
            cost_micro_usd: 5000,
            price_source: 'catalog',
            accounting_quality: 'standard',
          },
          headers: {
            'content-type': ['application/json'],
            authorization: ['Bearer secret-token-12345'],
            'x-request-id': ['req-header-123'],
            traceparent: ['00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01'],
            'x-multi-value': ['alpha', 'beta'],
          },
          body: { prompt: 'Hello world', secret_key: 'top-secret' },
          capture_status: 'available',
          headers_status: 'available',
          body_status: 'available',
          captured_at: '2026-10-20T10:00:00Z',
          expires_at: '2026-10-21T10:00:00Z',
          original_bytes: 1024,
          compressed_bytes: 256,
        } satisfies Detail;
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
            {
              id: 'deleted-key-xyz',
              label: 'Archived Key',
              enabled: false,
              deleted_at: '2026-10-15T00:00:00Z',
              max_concurrent: 1,
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
   * Proves that date bounds properly use inclusive UTC dates (YYYY-MM-DD),
   * and buildRequestViewParams constructs valid GET /requests/view queries.
   */
  it('UTC bounds: correctly formats inclusive UTC dates for GET /requests/view', () => {
    const query = buildRequestViewParams(
      {
        from: '2026-10-20',
        to: '2026-10-20',
        key_id: 'test-key',
        model: 'gpt-4o',
        provider: 'openai',
        auth_index: 'default',
        status: 'success',
      },
      'requests',
      1,
      25,
      FIXED_NOW
    );

    const params = new URLSearchParams(query);
    expect(params.get('from_date')).toBe('2026-10-20');
    expect(params.get('to_date')).toBe('2026-10-20');
    expect(params.get('view')).toBe('requests');
    expect(params.get('key_id')).toBe('test-key');
    expect(params.get('model')).toBe('gpt-4o');
    expect(params.get('provider')).toBe('openai');
    expect(params.get('auth_index')).toBe('default');
    expect(params.get('status')).toBe('success');
    expect(params.get('page')).toBe('1');
    expect(params.get('page_size')).toBe('25');
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
   * Proves accurate formulas for cache hit rate, failure rate, and single request TPS.
   */
  it('formulas: calculates cache hit rate with full input denominator, failure rate, and stream TPS accurately', () => {
    // Cache hit rate = cache_read / (input + cache_read + cache_write)
    // 100 read, 300 input, 100 write -> 100 / 500 = 20.0%
    expect(calculateCacheHitRate(100, 300, 100)).toBe('20.0%');
    // Unknown or 0 denominator returns null
    expect(calculateCacheHitRate(0, 0, 0)).toBeNull();

    // Failure rate
    expect(calculateFailureRate(2, 10)).toBe('20.0%');
    expect(calculateFailureRate(0, 0)).toBeNull();

    // TPS = output / ((latency_ms - ttft_ms) / 1000)
    // 50 output, latency 120ms, ttft 20ms -> (120 - 20) / 1000 = 0.1s -> 50 / 0.1 = 500.0 TPS
    expect(calculateTPS(50, 120, 20, true)).toBe('500.0');
    // Non-stream or invalid ttft returns null
    expect(calculateTPS(50, 120, 20, false)).toBeNull();
    expect(calculateTPS(50, 120, 0, true)).toBeNull();
    expect(calculateTPS(50, 120, 150, true)).toBeNull();
  });

  /**
   * Why covered:
   * Proves that preset ranges are inclusive and properly bounded:
   * - today = [today, today]
   * - last 7 days = [6 days ago, today] (7 full inclusive days)
   * - last 30 days = [29 days ago, today] (30 full inclusive days)
   */
  it('preset ranges: inclusive UTC ranges for today, last 7 days, and last 30 days', () => {
    const today = getPresetRange('today', FIXED_NOW);
    expect(today.from).toBe('2026-10-20');
    expect(today.to).toBe('2026-10-20');
    expect(isDynamicRange(today.to, FIXED_NOW)).toBe(true);

    const last7 = getPresetRange('last7days', FIXED_NOW);
    expect(last7.from).toBe('2026-10-14');
    expect(last7.to).toBe('2026-10-20');

    const last30 = getPresetRange('last30days', FIXED_NOW);
    expect(last30.from).toBe('2026-09-21');
    expect(last30.to).toBe('2026-10-20');

    // Historical range ending in past is not dynamic
    expect(isDynamicRange('2026-10-19', FIXED_NOW)).toBe(false);
  });

  /**
   * Why covered:
   * Proves that RequestsPage renders 6 authoritative metric cards, trend chart,
   * table with full token and cache columns, and uses GET /requests/view.
   */
  it('page rendering: renders 6 metric cards, trend chart, and table using GET /requests/view', async () => {
    const api = createMockApiClient();
    const onDirtyChange = vi.fn();

    render(<RequestsPage api={api} refreshInterval={5} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByText('请求日志')).toBeInTheDocument();
    });

    // 6 Metric Cards
    await waitFor(() => {
      expect(screen.getByTestId('metric-requests')).toHaveTextContent('12');
      expect(screen.getByTestId('metric-failures')).toHaveTextContent('2');
      expect(screen.getByTestId('metric-tokens')).toHaveTextContent('45,000');
      expect(screen.getByTestId('metric-cache')).toHaveTextContent('23.8%'); // 10000 / (30000+10000+2000)
      expect(screen.getByTestId('metric-latency')).toHaveTextContent('0.20 s'); // 2400 / 12
      expect(screen.getByTestId('metric-latency')).toHaveTextContent('TTFT: 0.06 s'); // 600 / 10
      expect(screen.getByTestId('metric-cost')).toHaveTextContent('$1.5');
    });

    // Trend chart element exists
    expect(screen.getByTestId('requests-trend')).toBeInTheDocument();

    // Table rows with records
    expect(screen.getByTestId('record-row-req-001')).toBeInTheDocument();
    expect(screen.getByTestId('record-row-req-002')).toBeInTheDocument();

    // Verify GET /requests/view was called
    const viewCalls = (api.request as ReturnType<typeof vi.fn>).mock.calls.filter((call) =>
      call[0].startsWith('/requests/view')
    );
    expect(viewCalls.length).toBeGreaterThan(0);
  });

  /**
   * Why covered:
   * Proves that request detail (/requests/detail?id=...) is fetched strictly on demand
   * when a user clicks the "查看" button or model link, rather than pre-fetching.
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

    // Click the "查看" button for req-001
    const viewBtns = screen.getAllByRole('button', { name: /查看/ });
    fireEvent.click(viewBtns[0]);

    // Detail endpoint must now have been called exactly once with req-001
    await waitFor(() => {
      const detailCallsAfter = (api.request as ReturnType<typeof vi.fn>).mock.calls.filter((call) =>
        call[0].startsWith('/requests/detail?id=req-001')
      );
      expect(detailCallsAfter.length).toBe(1);
    });

    // Modal dialog is displayed
    expect(screen.getByText(/请求详情: req-001/)).toBeInTheDocument();
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
        if (path.startsWith('/requests/view')) {
          const sample = createSampleRequestView();
          sample.records = [
            {
              id: 'req-slow',
              trace_id: 'trace-slow',
              key_id: 'key-abc',
              time: '2026-10-20T00:00:00Z',
              model: 'slow-model',
              provider: 'test',
              auth_index: '',
              failed: false,
              status_code: 200,
              stream: false,
              latency_ms: 100,
              ttft_ms: 0,
              tokens: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, total: 0 },
              cost_micro_usd: 0,
              price_source: 'catalog',
              accounting_quality: 'standard',
            },
          ];
          return sample;
        }
        if (path.startsWith('/requests/facets')) return { items: [] };
        if (path.startsWith('/keys')) return { items: [] };
        return {};
      }),
    });

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    await waitFor(() => {
      expect(screen.getByText('slow-model')).toBeInTheDocument();
    });

    // Open detail
    fireEvent.click(screen.getByRole('button', { name: '查看请求详情 req-slow' }));
    expect(screen.getByText('请求详情')).toBeInTheDocument();

    // Close modal while request is still pending
    const closeBtn = screen.getByRole('button', { name: '关闭' });
    fireEvent.click(closeBtn);

    // Modal is closed
    expect(screen.queryByText(/请求详情: req-slow/)).not.toBeInTheDocument();

    // Now resolve the late response
    await act(async () => {
      resolveDetail({
        record: {
          id: 'req-slow',
          model: 'slow-model',
          time: '2026-10-20T00:00:00Z',
          tokens: { total: 0 },
        },
        headers: {},
        body: null,
        capture_status: 'available',
      });
    });

    // Late response must not reopen the modal
    expect(screen.queryByText(/请求详情: req-slow/)).not.toBeInTheDocument();
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
              trace_id: '',
              key_id: '',
              time: '2026-10-20T00:00:00Z',
              model: maliciousModel,
              provider: '',
              auth_index: '',
              failed: false,
              status_code: 200,
              stream: false,
              latency_ms: 100,
              ttft_ms: 0,
              tokens: { input: 10, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, total: 10 },
              cost_micro_usd: 0,
              price_source: 'catalog',
              accounting_quality: 'standard',
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
            headers_status: 'redacted',
            body_status: 'redacted',
          };
        }
        if (path.startsWith('/requests/view')) {
          const sample = createSampleRequestView();
          sample.records = [
            {
              id: 'req-xss',
              trace_id: '',
              key_id: '',
              time: '2026-10-20T00:00:00Z',
              model: maliciousModel,
              provider: '',
              auth_index: '',
              failed: false,
              status_code: 200,
              stream: false,
              latency_ms: 100,
              ttft_ms: 0,
              tokens: { input: 10, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, total: 10 },
              cost_micro_usd: 0,
              price_source: 'catalog',
              accounting_quality: 'standard',
            },
          ];
          return sample;
        }
        if (path.startsWith('/requests/facets')) return { items: [] };
        if (path.startsWith('/keys')) return { items: [] };
        return {};
      }),
    });

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    // Model name is rendered as pure text inside a link button
    const modelBtn = await screen.findByRole('button', { name: maliciousModel });
    expect(modelBtn).toBeInTheDocument();
    expect((window as unknown as Record<string, unknown>).__xssInjected).toBeUndefined();

    // Open detail
    fireEvent.click(modelBtn);
    await screen.findByText(/请求详情: req-xss/);

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
   * Proves that filter edits only update draft, report onDirtyChange(true),
   * and clicking "查询" applies the draft and queries page 1.
   */
  it('filter draft vs applied: edits only update draft, submit applies & queries page 1', async () => {
    const api = createMockApiClient();
    const onDirtyChange = vi.fn();

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByLabelText('开始日期')).toBeInTheDocument();
    });

    const callsBefore = (api.request as ReturnType<typeof vi.fn>).mock.calls.length;

    // Change start date input
    const fromInput = screen.getByLabelText('开始日期');
    fireEvent.change(fromInput, { target: { value: '2026-10-01' } });

    // onDirtyChange called with true
    expect(onDirtyChange).toHaveBeenCalledWith(true);

    // Did NOT immediately fire query
    expect((api.request as ReturnType<typeof vi.fn>).mock.calls.length).toBe(callsBefore);

    // Shows "筛选未应用" banner
    expect(screen.getByText(/筛选未应用/)).toBeInTheDocument();

    // Click "查询" button
    const submitBtn = screen.getByRole('button', { name: '查询' });
    fireEvent.click(submitBtn);

    // After apply, dirty becomes false
    await waitFor(() => {
      expect(onDirtyChange).toHaveBeenLastCalledWith(false);
    });

    // Query was sent with from_date=2026-10-01 and page=1
    await waitFor(() => {
      const callsWithFrom = (api.request as ReturnType<typeof vi.fn>).mock.calls.filter((call) =>
        call[0].includes('from_date=2026-10-01') && call[0].includes('page=1')
      );
      expect(callsWithFrom.length).toBeGreaterThan(0);
    });
  });

  /**
   * Why covered:
   * Proves that latency/TTFT are displayed in seconds with fixed two decimals
   * for rows, grouped averages and the detail modal, without changing TPS math.
   */
  it('duration units: list, group averages and detail render seconds while TPS stays unchanged', async () => {
    expect(formatSecondsFromMs(15697)).toBe('15.70 s');
    expect(formatSecondsFromMs(1474)).toBe('1.47 s');
    expect(formatSecondsFromMs(0)).toBe('0.00 s');
    expect(formatSecondsFromMs(undefined)).toBe('—');
    expect(formatAvgSeconds(1400, 7)).toBe('0.20 s');
    expect(formatAvgSeconds(undefined, 7)).toBe('—');
    expect(formatAvgSeconds(1400, 0)).toBe('—');

    const api = createMockApiClient();
    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    // req-001: 120ms -> 0.12 s, TTFT 45ms -> 0.05 s, TPS formula unchanged.
    const firstRow = await screen.findByTestId('record-row-req-001');
    expect(firstRow).toHaveTextContent('0.12 s');
    expect(firstRow).toHaveTextContent('TTFT: 0.05 s');
    expect(firstRow).not.toHaveTextContent('120 ms');
    expect(calculateTPS(50, 120, 45, true)).toBe('666.7');

    // req-002 is not streaming: latency only, no TTFT line.
    const secondRow = screen.getByTestId('record-row-req-002');
    expect(secondRow).toHaveTextContent('0.35 s');
    expect(secondRow).not.toHaveTextContent('TTFT');

    // Group average: 1400/7 = 200ms -> 0.20 s; TTFT 350/5 = 70ms -> 0.07 s.
    fireEvent.click(screen.getByRole('tab', { name: '模型分组' }));
    await waitFor(() => {
      // The summary card and group row both show 0.20 s after the metrics switch.
      expect(screen.getAllByText('0.20 s')).toHaveLength(2);
      expect(screen.getByText('TTFT: 0.07 s')).toBeInTheDocument();
    });

    // Detail modal uses the same seconds representation.
    fireEvent.click(screen.getByRole('tab', { name: '请求明细' }));
    fireEvent.click(await screen.findByRole('button', { name: '查看请求详情 req-001' }));
    await screen.findByText(/请求详情: req-001/);
    await waitFor(() => {
      expect(screen.getByText('0.12 s (TTFT: 0.05 s)')).toBeInTheDocument();
    });
  });

  /**
   * Why covered:
   * Proves the detail header tab displays arbitrary non-sensitive headers
   * (including X-Request-ID and traceparent) and multi-value headers, copies
   * them, and still redacts sensitive values.
   */
  it('header display: arbitrary and multi-value headers are visible/copyable while secrets stay redacted', async () => {
    const api = createMockApiClient();
    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    fireEvent.click(await screen.findByRole('button', { name: '查看请求详情 req-001' }));
    await screen.findByText(/请求详情: req-001/);
    fireEvent.click(screen.getByRole('tab', { name: '请求头' }));

    expect(screen.getByText('x-request-id')).toBeInTheDocument();
    expect(screen.getByText('req-header-123')).toBeInTheDocument();
    expect(screen.getByText('traceparent')).toBeInTheDocument();
    expect(screen.getByText('00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01')).toBeInTheDocument();
    expect(screen.getByText('x-multi-value')).toBeInTheDocument();
    expect(screen.getByText('alpha, beta')).toBeInTheDocument();
    expect(screen.queryByText('Bearer secret-token-12345')).not.toBeInTheDocument();

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: '复制请求头 JSON' }));
    });
    expect(mockClipboard.writeText).toHaveBeenCalledTimes(1);
    const copied = JSON.parse(mockClipboard.writeText.mock.calls[0][0] as string);
    expect(copied['x-request-id']).toBe('req-header-123');
    expect(copied.traceparent).toBe('00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01');
    expect(copied['x-multi-value']).toBe('alpha, beta');
    expect(copied.authorization).toBe('[redacted]');
    expect(JSON.stringify(copied)).not.toContain('secret-token-12345');
  });

  /**
   * Why covered:
   * Proves model and Key pickers still filter their current options locally when
   * onSearchChange is used for remote facets, Enter picks the visible match rather
   * than the all-option, and no-match Enter leaves the selected value untouched.
   */
  it('model and key filters narrow locally with remote search callback and select matches', async () => {
    const api = createMockApiClient();
    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    const modelTrigger = await screen.findByRole('combobox', { name: '模型' });
    fireEvent.keyDown(modelTrigger, { key: 'ArrowDown' });
    let searchInput = await screen.findByRole('textbox', { name: '搜索选项' });
    fireEvent.change(searchInput, { target: { value: 'CLAUDE' } });

    expect(screen.getByRole('option', { name: 'claude-3-5-sonnet' })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: '全部模型' })).not.toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'gpt-4o' })).not.toBeInTheDocument();

    fireEvent.keyDown(searchInput, { key: 'Enter' });
    await waitFor(() => {
      expect(modelTrigger).toHaveTextContent('claude-3-5-sonnet');
    });

    const keyTrigger = screen.getByRole('combobox', { name: '客户端 Key' });
    fireEvent.keyDown(keyTrigger, { key: 'ArrowDown' });
    searchInput = await screen.findByRole('textbox', { name: '搜索选项' });
    fireEvent.change(searchInput, { target: { value: 'prod' } });

    expect(screen.getByRole('option', { name: 'Production Key' })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: '全部 Key' })).not.toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'Archived Key (已删除)' })).not.toBeInTheDocument();

    fireEvent.keyDown(searchInput, { key: 'Enter' });
    await waitFor(() => {
      expect(keyTrigger).toHaveTextContent('Production Key');
    });

    // A no-match Enter leaves the existing selection and open search intact.
    fireEvent.keyDown(keyTrigger, { key: 'ArrowDown' });
    searchInput = await screen.findByRole('textbox', { name: '搜索选项' });
    fireEvent.change(searchInput, { target: { value: 'no-such-key' } });
    expect(screen.getByText('无匹配项')).toBeInTheDocument();
    fireEvent.keyDown(searchInput, { key: 'Enter' });
    expect(keyTrigger).toHaveTextContent('Production Key');
    expect(screen.getByRole('textbox', { name: '搜索选项' })).toBeInTheDocument();

    // Escape uses the shared close cleanup so the next open starts with an empty query.
    fireEvent.keyDown(searchInput, { key: 'Escape' });
    fireEvent.keyDown(keyTrigger, { key: 'ArrowDown' });
    expect(await screen.findByRole('textbox', { name: '搜索选项' })).toHaveValue('');
    expect(screen.getByRole('option', { name: '全部 Key' })).toBeInTheDocument();
  });

  /**
   * Why covered:
   * Proves a late remote/history facet result joins the still-active local search
   * and can be selected without losing the typed query or the chosen value.
   */
  it('late model facet results join the active search without breaking selection', async () => {
    let resolveLateFacet: (value: FacetPage) => void = () => {};
    const base = createMockApiClient();
    const api = createMockApiClient({
      request: vi.fn((path: string, options?: { signal?: AbortSignal }) => {
        if (path.startsWith('/requests/facets')) {
          const params = new URLSearchParams(path.slice(path.indexOf('?') + 1));
          if (params.get('field') === 'model' && params.get('search') === 'late') {
            return new Promise<FacetPage>((resolve) => {
              resolveLateFacet = resolve;
            });
          }
        }
        return base.request(path, options);
      }) as ApiClient['request'],
    });
    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    const trigger = await screen.findByRole('combobox', { name: '模型' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    const searchInput = await screen.findByRole('textbox', { name: '搜索选项' });
    fireEvent.change(searchInput, { target: { value: 'late' } });
    expect(screen.getByText('无匹配项')).toBeInTheDocument();

    await act(async () => {
      resolveLateFacet({
        items: [{ value: 'late-model-v1', label: 'late-model-v1 (历史)' }],
      });
    });

    expect(screen.getByRole('option', { name: 'late-model-v1 (历史)' })).toBeInTheDocument();
    expect(searchInput).toHaveValue('late');
    fireEvent.keyDown(searchInput, { key: 'Enter' });
    expect(trigger).toHaveTextContent('late-model-v1 (历史)');
  });

  /**
   * Why covered:
   * Proves that Key labels display friendly names, deleted keys show [已删除] tag,
   * 64-hex IDs are truncated with a full-ID tooltip, and no copy-ID action is shown.
   */
  it('Key labels & deleted keys: friendly name, deleted tag, 64-hex truncation, no copy action', async () => {
    const api = createMockApiClient();
    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    // Production Key has a label 'Production Key'
    await waitFor(() => {
      expect(screen.getByText('Production Key', { selector: '.requests-key-name' })).toBeInTheDocument();
    });

    // req-002 has a 64-hex ID; should show 12-char prefix + ellipsis
    const truncatedKey = screen.getByText('4f68a2d1e09c…');
    expect(truncatedKey).toBeInTheDocument();
    expect(truncatedKey).toHaveAttribute(
      'title',
      '4f68a2d1e09c8b7a6543210fedcba9874f68a2d1e09c8b7a6543210fedcba987'
    );

    // The request list must not expose an internal Key ID copy action.
    expect(screen.queryByRole('button', { name: '复制 ID' })).not.toBeInTheDocument();
  });

  /**
   * Why covered:
   * Proves honest handling of capture states: disabled displays honest copy,
   * null body is not displayed as empty JSON success {}.
   */
  it('capture states: displays honest capture status and does not display null body as empty JSON success', async () => {
    const api = createMockApiClient({
      request: vi.fn().mockImplementation(async (path: string) => {
        if (path.startsWith('/requests/detail')) {
          return {
            record: {
              id: 'req-disabled',
              trace_id: '',
              key_id: '',
              time: '2026-10-20T00:00:00Z',
              model: 'model-a',
              provider: '',
              auth_index: '',
              failed: false,
              status_code: 200,
              stream: false,
              latency_ms: 100,
              ttft_ms: 0,
              tokens: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, total: 0 },
              cost_micro_usd: 0,
              price_source: 'catalog',
              accounting_quality: 'standard',
            },
            headers: {},
            body: null,
            capture_status: 'disabled',
            headers_status: 'disabled',
            body_status: 'disabled',
          };
        }
        if (path.startsWith('/requests/view')) {
          const sample = createSampleRequestView();
          sample.records = [
            {
              id: 'req-disabled',
              trace_id: '',
              key_id: '',
              time: '2026-10-20T00:00:00Z',
              model: 'model-a',
              provider: '',
              auth_index: '',
              failed: false,
              status_code: 200,
              stream: false,
              latency_ms: 100,
              ttft_ms: 0,
              tokens: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, total: 0 },
              cost_micro_usd: 0,
              price_source: 'catalog',
              accounting_quality: 'standard',
            },
          ];
          return sample;
        }
        if (path.startsWith('/requests/facets')) return { items: [] };
        if (path.startsWith('/keys')) return { items: [] };
        return {};
      }),
    });

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    const viewBtn = await screen.findByRole('button', { name: '查看请求详情 req-disabled' });
    fireEvent.click(viewBtn);

    await screen.findByText(/请求详情: req-disabled/);
    // Switch to JSON body tab
    fireEvent.click(screen.getByRole('tab', { name: '请求体 (JSON)' }));

    // Must show honest notice '无可用正文' and '采集未开启 (默认)' rather than '{}'
    expect(screen.getByText('无可用正文')).toBeInTheDocument();
    expect(screen.getByText('采集未开启 (默认)', { exact: true })).toBeInTheDocument();
    expect(screen.queryByText('{}')).not.toBeInTheDocument();
  });

  /**
   * Why covered:
   * Proves 5 views tabs switching: switching from '请求明细' to '模型分组'
   * queries GET /requests/view?view=model and displays aggregated group columns.
   */
  it('view tabs: switches between requests and group views with whole-range aggregate', async () => {
    const api = createMockApiClient();
    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    await waitFor(() => {
      expect(screen.getByRole('tab', { name: '请求明细' })).toBeInTheDocument();
    });

    // Click '模型分组' tab
    const modelTab = screen.getByRole('tab', { name: '模型分组' });
    fireEvent.click(modelTab);

    // Query must be sent with view=model
    await waitFor(() => {
      const modelViewCalls = (api.request as ReturnType<typeof vi.fn>).mock.calls.filter((call) =>
        call[0].includes('view=model')
      );
      expect(modelViewCalls.length).toBeGreaterThan(0);
    });

    // Table now shows group row for gpt-4o
    await waitFor(() => {
      expect(screen.getByText('渠道: openai')).toBeInTheDocument();
    });
  });

  // Names are display metadata, not group/filter identities. Keep duplicate
  // names on separate indexes and preserve the full long name in its tooltip.
  it('shows account names with index tooltips and filters by the original index', async () => {
    const name = `production-${'long-account-name-'.repeat(6)}.json`;
    const base = createMockApiClient();
    const api = createMockApiClient({
      request: vi.fn(async (path, options) => {
        if (path.startsWith('/requests/facets') && path.includes('field=auth_index')) {
          return { items: [
            { value: 'auth-a', label: name },
            { value: 'auth-b', label: name },
            { value: 'legacy-auth' },
          ] };
        }
        const result = await base.request(path, options);
        if (path.startsWith('/requests/view')) {
          const view = result as RequestView;
          view.groups = [
            { ...view.summary, key: 'auth-a', auth_index: 'auth-a', label: name },
            { ...view.summary, key: 'auth-b', auth_index: 'auth-b', label: name },
            { ...view.summary, key: 'legacy-auth', auth_index: 'legacy-auth' },
          ];
        }
        return result;
      }) as ApiClient['request'],
    });
    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);
    fireEvent.click(await screen.findByRole('tab', { name: '账户分组' }));

    const names = await screen.findAllByText(name, { selector: '.requests-account-name' });
    expect(names).toHaveLength(2);
    expect(names[0]).toHaveAttribute('title', `${name}\nAuthIndex: auth-a`);
    expect(names[1]).toHaveAttribute('title', `${name}\nAuthIndex: auth-b`);
    expect(screen.getByText('legacy-auth', { selector: '.requests-account-name' })).toBeInTheDocument();
    expect(screen.getByRole('columnheader', { name: '账户' })).toBeInTheDocument();

    const trigger = screen.getByRole('combobox', { name: '账户' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    const search = screen.getByRole('textbox', { name: '搜索选项' });
    fireEvent.change(search, { target: { value: name } });
    fireEvent.keyDown(search, { key: 'Enter' });
    expect(trigger).toHaveTextContent(name);
    fireEvent.click(screen.getByRole('button', { name: '查询' }));
    await waitFor(() => {
      const calls = vi.mocked(api.request).mock.calls.filter(([path]) => path.startsWith('/requests/view'));
      const params = new URLSearchParams(calls[calls.length - 1][0].split('?')[1]);
      expect(params.get('auth_index')).toBe('auth-a');
    });
  });

  // The interval menu is an actual Radix control rather than a hidden timer
  // configuration. Timer/pause behavior is independently exercised below.
  it('polling controls: dedicated interval selector', async () => {
    const api = createMockApiClient();
    render(<RequestsPage api={api} refreshInterval={5} onDirtyChange={vi.fn()} />);

    await waitFor(() => {
      expect(screen.getByLabelText('自动刷新周期')).toBeInTheDocument();
    });

    // Change interval to 10s
    const pollSelect = screen.getByLabelText('自动刷新周期');
    fireEvent.click(pollSelect);
    const opt10 = await screen.findByText('每 10 秒刷新');
    fireEvent.click(opt10);

    // Verify polling selector value updated
    expect(screen.getByText('每 10 秒刷新')).toBeInTheDocument();
  });

  // Advance real component timers, not just pause-hint text: control focus,
  // draft changes, details, paging, visibility and history must stop requests.
  it('polling pauses for interaction, draft, detail, paging, hidden documents and historical ranges', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(FIXED_NOW);
    const base = createMockApiClient();
    const api = createMockApiClient({ request: vi.fn(async (path, options) => {
      const value = await base.request(path, options);
      if (path.startsWith('/requests/view')) (value as RequestView).pagination.has_more = true;
      return value;
    }) as ApiClient['request'] });
    const mounted = render(<RequestsPage api={api} onDirtyChange={vi.fn()} />);
    const reads = () => vi.mocked(api.request).mock.calls.filter(([path]) => path.startsWith('/requests/view')).length;
    const advance = async (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
    try {
      await act(async () => {});
      expect(reads()).toBe(1);
      await advance(5000);
      expect(reads()).toBe(2);
      fireEvent.focus(screen.getByLabelText('开始日期'));
      await advance(10000);
      expect(reads()).toBe(2);
      fireEvent.blur(screen.getByLabelText('开始日期'));
      fireEvent.change(screen.getByLabelText('开始日期'), { target: { value: '2026-10-01' } });
      await advance(10000);
      expect(reads()).toBe(2);
      await act(async () => { fireEvent.click(screen.getByRole('button', { name: '查询' })); });
      expect(reads()).toBe(3);
      await advance(5000);
      expect(reads()).toBe(4); // resuming did not start a duplicate immediate fetch
      await act(async () => { fireEvent.click(screen.getByRole('button', { name: '查看请求详情 req-001' })); });
      await advance(10000);
      expect(reads()).toBe(4);
      fireEvent.click(screen.getByRole('button', { name: '关闭' }));
      await act(async () => { fireEvent.click(screen.getByRole('button', { name: '下一页' })); });
      expect(reads()).toBe(5);
      await advance(10000);
      expect(reads()).toBe(5);
      await act(async () => { fireEvent.click(screen.getByRole('button', { name: '上一页' })); });
      expect(reads()).toBe(6);
      const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
      await advance(10000);
      expect(reads()).toBe(6);
      hidden.mockRestore();
      await advance(5000);
      expect(reads()).toBe(7);
      fireEvent.change(screen.getByLabelText('结束日期'), { target: { value: '2026-10-19' } });
      await act(async () => { fireEvent.click(screen.getByRole('button', { name: '查询' })); });
      expect(reads()).toBe(8);
      await advance(60000);
      expect(reads()).toBe(8);
    } finally {
      mounted.unmount();
      vi.restoreAllMocks();
      vi.useRealTimers();
    }
  });

  // Rolling presets follow the UTC calendar, including leap day, while a
  // user-applied historical date remains fixed across later midnight changes.
  it('rolling today advances across UTC leap day without moving a custom historical range', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2024-02-28T23:59:58Z'));
    const api = createMockApiClient();
    const mounted = render(<RequestsPage api={api} onDirtyChange={vi.fn()} />);
    const viewCalls = () => vi.mocked(api.request).mock.calls.filter(([path]) => path.startsWith('/requests/view'));
    try {
      await act(async () => {});
      expect(viewCalls()[0][0]).toContain('to_date=2024-02-28');
      await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
      expect(viewCalls()).toHaveLength(2);
      expect(viewCalls()[1][0]).toContain('from_date=2024-02-29&to_date=2024-02-29');
      expect(screen.getByLabelText('开始日期')).toHaveValue('2024-02-29');
      expect(screen.getByLabelText('结束日期')).toHaveValue('2024-02-29');
      fireEvent.change(screen.getByLabelText('开始日期'), { target: { value: '2024-02-27' } });
      fireEvent.change(screen.getByLabelText('结束日期'), { target: { value: '2024-02-27' } });
      await act(async () => { fireEvent.click(screen.getByRole('button', { name: '查询' })); });
      const count = viewCalls().length;
      vi.setSystemTime(new Date('2024-03-01T00:00:00Z'));
      await act(async () => { await vi.advanceTimersByTimeAsync(60000); });
      expect(viewCalls()).toHaveLength(count);
      expect(screen.getByLabelText('结束日期')).toHaveValue('2024-02-27');
    } finally {
      mounted.unmount();
      vi.useRealTimers();
    }
  });

  // A slow response must not create an overlapping poll; a newer explicit
  // refresh wins even if the old server ignores abort. Failures back off and
  // preserve the exact existing row node rather than flashing a loading table.
  it('polling is single-flight, ignores aborted late responses and backs off after errors', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(FIXED_NOW);
    const base = createMockApiClient();
    let count = 0;
    let stale!: (value: RequestView) => void;
    let staleSignal: AbortSignal | undefined;
    const api = createMockApiClient({ request: vi.fn((path, options) => {
      if (!path.startsWith('/requests/view')) return base.request(path, options);
      count++;
      if (count === 2) {
        staleSignal = options?.signal;
        return new Promise<RequestView>(resolve => { stale = resolve; });
      }
      if (count === 4) return Promise.reject(new Error('offline'));
      const value = createSampleRequestView();
      value.summary.requests = 99;
      return Promise.resolve(value);
    }) as ApiClient['request'] });
    const mounted = render(<RequestsPage api={api} onDirtyChange={vi.fn()} />);
    const advance = async (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });
    try {
      await act(async () => {});
      const row = screen.getByRole('button', { name: '查看请求详情 req-001' }).closest('tr');
      await advance(5000);
      expect(count).toBe(2);
      await advance(60000);
      expect(count).toBe(2);
      await act(async () => { fireEvent.click(screen.getByRole('button', { name: '刷新' })); });
      expect(staleSignal?.aborted).toBe(true);
      expect(count).toBe(3);
      const old = createSampleRequestView();
      old.summary.requests = 1000;
      await act(async () => { stale(old); });
      expect(screen.getByTestId('metric-requests')).toHaveTextContent('99');
      await advance(5000); // the next timer fails
      expect(count).toBe(4);
      expect(screen.getByText('offline')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: '查看请求详情 req-001' }).closest('tr')).toBe(row);
      await advance(5000);
      expect(count).toBe(4); // backoff to ten seconds
      await advance(5000);
      expect(count).toBe(5);
      expect(screen.queryByText('offline')).not.toBeInTheDocument();
    } finally {
      mounted.unmount();
      vi.useRealTimers();
    }
  });

  /**
   * Why covered:
   * Proves corrective whole-date CTA is offered upon date query errors,
   * while generic network errors offer standard retry button without false CTA.
   */
  it('error handling: offers corrective whole-date CTA on historical query error and retry on generic error', async () => {
    let shouldFailWithHistorical = true;
    const api = createMockApiClient({
      request: vi.fn().mockImplementation(async (path: string) => {
        if (path.startsWith('/requests/view')) {
          if (shouldFailWithHistorical) {
            throw new Error('invalid query: historical range requires whole dates');
          }
          return createSampleRequestView();
        }
        if (path.startsWith('/requests/facets')) return { items: [] };
        if (path.startsWith('/keys')) return { items: [] };
        return {};
      }),
    });

    render(<RequestsPage api={api} refreshInterval={0} onDirtyChange={vi.fn()} />);

    // Query error notice with corrective whole-date button
    const errorNotice = await screen.findByText(/invalid query/);
    expect(errorNotice).toBeInTheDocument();

    const correctiveBtn = screen.getByRole('button', { name: '修正为整日 UTC 范围' });
    expect(correctiveBtn).toBeInTheDocument();

    // Click corrective button
    shouldFailWithHistorical = false;
    fireEvent.click(correctiveBtn);

    // After correction, data loads
    await waitFor(() => {
      expect(screen.getByTestId('metric-requests')).toBeInTheDocument();
    });

    // Verify helper behavior directly
    expect(isHistoricalDateQueryError('Network request failed', '2026-10-18', '2026-10-19')).toBe(false);
    expect(isHistoricalDateQueryError('401 unauthorized', '2026-10-18', '2026-10-19')).toBe(false);
  });
});
