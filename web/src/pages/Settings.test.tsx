import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { SettingsPage } from './Settings';
import type { ApiClient } from '../lib/api';
import type { Settings, PriceSnapshot, Health, ModelInfo, PricePreview } from '../lib/types';

describe('SettingsPage', () => {
  let mockApi: ApiClient;
  let onDirtyChange: ReturnType<typeof vi.fn>;

  const initialSettings: Settings = {
    capture_enabled: false,
    max_body_bytes: 1048576,
    max_body_storage_bytes: 268435456,
    raw_retention_hours: 24,
    daily_retention_days: 365,
    pricing: {
      auto: true,
      prices: [
        {
          model: 'claude-3-5-sonnet',
          rates: { input: 3000000, output: 15000000, cache_read: 300000, cache_write: 3750000, reasoning: 0 },
          source: 'manual',
        },
      ],
      rules: [
        {
          id: 'rule-night',
          model: 'claude-3-5-sonnet',
          time_range: '00:00-08:00',
          rates: { input: 1500000, output: 7500000, cache_read: 150000, cache_write: 1875000, reasoning: null },
        },
      ],
    },
  };

  const initialSnapshot: PriceSnapshot = {
    prices: [
      {
        model: 'gpt-4o',
        provider: 'openai',
        rates: { input: 2500000, output: 10000000, cache_read: 1250000, cache_write: 2500000, reasoning: 0 },
        source: 'auto',
      },
    ],
    last_sync: '2026-10-18T12:00:00Z',
    refreshing: false,
    catalog_entries: 1500,
  };

  const initialHealth: Health = {
    database_bytes: 15728640, // 15 MB
    dropped_captures: 0,
    pending_usage: 0,
    active_requests: 2,
    last_error: '',
  };

  const initialModels: ModelInfo[] = [
    { id: 'gpt-4o', owned_by: 'openai' },
    { id: 'custom-unpriced-model' },
  ];

  const initialPreview: PricePreview = {
    items: [
      {
        model: 'gpt-4o',
        provider: 'openai',
        rates: { input: 2500000, output: 10000000, cache_read: 1250000, cache_write: 2500000, reasoning: 0 },
        source: 'auto',
        has_rules: false,
      },
      {
        model: 'custom-unpriced-model',
        rates: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: undefined },
        source: 'unpriced',
        has_rules: true,
      },
    ],
  };

  beforeEach(() => {
    onDirtyChange = vi.fn();
    const mockRequest = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
      if (path === '/settings') {
        if (options?.method === 'PUT') {
          return options.body as Settings;
        }
        return initialSettings;
      }
      if (path === '/prices') {
        return initialSnapshot;
      }
      if (path === '/prices/refresh') {
        return { snapshot: { ...initialSnapshot, catalog_entries: 1520 } };
      }
      if (path === '/prices/preview') {
        return initialPreview;
      }
      if (path === '/health') {
        return initialHealth;
      }
      if (path === '/compact') {
        return { health: { ...initialHealth, database_bytes: 10485760 } };
      }
      throw new Error(`Unhandled mock path: ${path}`);
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  // Test that all 3 task sections render properly on initial load
  it('renders all three task sections: 请求记录, 模型价格, 存储维护', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    // Shows loading notice initially
    expect(screen.getByTestId('settings-loading')).toBeDefined();

    // After resolution, loads main container
    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    expect(screen.getByRole('heading', { name: '请求记录' })).toBeDefined();
    expect(screen.getByRole('heading', { name: '模型价格' })).toBeDefined();
    expect(screen.getByRole('heading', { name: '存储维护' })).toBeDefined();

    // Verify accounting normal state displayed
    expect(screen.getByText('计费与存储状态正常')).toBeDefined();
    expect(screen.getAllByText('15.0 MB').length).toBeGreaterThanOrEqual(1);
  });

  // Test section draft isolation: saving General does NOT save unsaved Pricing drafts
  it('enforces section draft isolation: saving general settings preserves confirmed pricing', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // 1. Modify Pricing draft (e.g. toggle auto off)
    const pricingAutoSwitch = screen.getByLabelText('自动使用 models.dev 公共价格目录');
    fireEvent.click(pricingAutoSwitch);

    // Verify dirty callback notified
    expect(onDirtyChange).toHaveBeenCalledWith(true);

    // 2. Modify General draft (e.g. toggle capture on)
    const captureSwitch = screen.getByLabelText('保存请求内容');
    fireEvent.click(captureSwitch);

    // 3. Click Save General Settings
    const saveGeneralBtn = screen.getByTestId('settings-general-save');
    fireEvent.click(saveGeneralBtn);

    await waitFor(() => {
      // Find the PUT request made for /settings
      const putCalls = (mockApi.request as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call) => call[0] === '/settings' && call[1]?.method === 'PUT'
      );
      expect(putCalls.length).toBe(1);
      const payload = putCalls[0][1].body as Settings;
      expect(payload.capture_enabled).toBe(true);
      // Pricing sent to server MUST be confirmedSettings.pricing (auto: true), NOT the unsaved draft (auto: false)!
      expect(payload.pricing.auto).toBe(true);
    });
  });

  // Test section draft isolation: saving Pricing preserves confirmed General settings and preserves reasoning: 0
  it('enforces section draft isolation: saving pricing preserves confirmed general settings and preserves explicit reasoning 0', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // 1. Modify General draft (toggle capture on)
    const captureSwitch = screen.getByLabelText('保存请求内容');
    fireEvent.click(captureSwitch);

    // 2. Modify Pricing draft (add a manual price row with explicit reasoning 0)
    const addManualBtn = screen.getByTestId('manual-price-add');
    fireEvent.click(addManualBtn);

    const newModelInput = screen.getByLabelText('第 2 条手工价格模型 ID');
    fireEvent.change(newModelInput, { target: { value: 'deepseek-chat' } });

    // Set reasoning to explicit 0
    const newReasoningInput = screen.getByLabelText('第 2 条推理价格');
    fireEvent.change(newReasoningInput, { target: { value: '0' } });

    // 3. Save Pricing Settings
    const savePricingBtn = screen.getByTestId('settings-pricing-save');
    fireEvent.click(savePricingBtn);

    await waitFor(() => {
      const putCalls = (mockApi.request as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call) => call[0] === '/settings' && call[1]?.method === 'PUT'
      );
      expect(putCalls.length).toBe(1);
      const payload = putCalls[0][1].body as Settings;
      // General settings sent MUST be confirmed (capture_enabled: false), NOT the unsaved draft (true)!
      expect(payload.capture_enabled).toBe(false);
      // Pricing includes the new manual price with reasoning explicitly 0
      const deepseekPrice = payload.pricing.prices.find((p) => p.model === 'deepseek-chat');
      expect(deepseekPrice).toBeDefined();
      expect(deepseekPrice?.rates.reasoning).toBe(0);
    });
  });

  // Test dirty safety: cancelling drafts resets state and clears dirty indicator
  it('restores confirmed snapshot and resets dirty state when cancelling drafts', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // Modify General
    const captureSwitch = screen.getByLabelText('保存请求内容');
    fireEvent.click(captureSwitch);

    expect(screen.getByTestId('settings-general-reload')).toBeDefined();

    // Cancel General
    fireEvent.click(screen.getByTestId('settings-general-reload'));

    await waitFor(() => {
      expect(onDirtyChange).toHaveBeenLastCalledWith(false);
      expect(screen.queryByTestId('settings-general-reload')).toBeNull();
    });
  });

  // Test preview table display, unpriced warning, has_rules badge, and falsey reasoning 0 display
  it('displays available model prices preview with unpriced warning, rule badges, and explicit reasoning 0', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // Check preview call was made with CPA model IDs
    expect(mockApi.request).toHaveBeenCalledWith(
      '/prices/preview',
      expect.objectContaining({
        method: 'POST',
        body: {
          models: [
            { model: 'gpt-4o', provider: 'openai' },
            { model: 'custom-unpriced-model', provider: '' },
          ],
        },
      })
    );

    // gpt-4o row: rates displayed, and reasoning: 0 displayed as $0 (not falsey hidden)
    expect(screen.getByText('gpt-4o')).toBeDefined();
    expect(screen.getByText('$2.5')).toBeDefined();
    expect(screen.getByText('$10')).toBeDefined();
    expect(screen.getAllByText('$0').length).toBeGreaterThanOrEqual(1);

    // custom-unpriced-model row shows distinct warning and rule annotation
    expect(screen.getByText('custom-unpriced-model')).toBeDefined();
    expect(screen.getByText('未定价（按 $0 计费）')).toBeDefined();
    expect(screen.getByText('含条件规则')).toBeDefined();
  });

  // Test catalog refresh failure displays honest error and retains cache
  it('honestly displays catalog refresh failure without false success', async () => {
    const mockRequest = vi.fn(async (path: string) => {
      if (path === '/settings') return initialSettings;
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth;
      if (path === '/prices/preview') return initialPreview;
      if (path === '/prices/refresh') {
        // HTTP 200 but snapshot.error is present
        return {
          snapshot: {
            ...initialSnapshot,
            error: 'models.dev upstream timeout',
          },
        };
      }
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    const refreshBtn = screen.getByTestId('prices-refresh');
    fireEvent.click(refreshBtn);

    await waitFor(() => {
      // Must NOT render "已刷新"
      expect(screen.queryByText('价格目录已刷新')).toBeNull();
      // Must display honest error
      expect(screen.getByText(/价格目录刷新失败，已保留有效缓存：models\.dev upstream timeout/)).toBeDefined();
      // Existing cache must still be shown
      expect(screen.getByText(/公共目录总收录：1,500 条/)).toBeDefined();
    });
  });

  // Test storage maintenance compact cancellation and confirmation
  it('supports compact confirmation dialog and honors cancellation without firing compact API', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    const compactBtn = screen.getByTestId('health-compact');
    fireEvent.click(compactBtn);

    // Modal dialog is displayed
    expect(screen.getByText(/确定要执行数据库空间整理吗？/)).toBeDefined();

    // Click Cancel
    const cancelBtn = screen.getByTestId('compact-cancel');
    fireEvent.click(cancelBtn);

    // Modal closes
    await waitFor(() => {
      expect(screen.queryByText(/确定要执行数据库空间整理吗？/)).toBeNull();
    });
    // /compact API was NOT called
    expect(mockApi.request).not.toHaveBeenCalledWith('/compact', expect.anything());

    // Click compact again and confirm
    fireEvent.click(compactBtn);
    const confirmBtn = screen.getByTestId('compact-confirm');
    fireEvent.click(confirmBtn);

    await waitFor(() => {
      expect(mockApi.request).toHaveBeenCalledWith('/compact', { method: 'POST' });
      expect(screen.getByText('数据库压缩整理完成')).toBeDefined();
      // Database size updated from 15MB to 10MB
      expect(screen.getAllByText('10.0 MB').length).toBeGreaterThanOrEqual(1);
    });
  });

  // Test rule ordering operations: up, down, remove
  it('manages rule ordering with up/down controls and preserves order in save payload', async () => {
    const multiRuleSettings: Settings = {
      ...initialSettings,
      pricing: {
        auto: true,
        prices: [],
        rules: [
          {
            id: 'rule-1',
            model: 'model-a',
            rates: { input: 1000000, output: 2000000, cache_read: 0, cache_write: 0 },
          },
          {
            id: 'rule-2',
            model: 'model-b',
            rates: { input: 3000000, output: 4000000, cache_read: 0, cache_write: 0 },
          },
        ],
      },
    };

    const mockRequest = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
      if (path === '/settings') {
        if (options?.method === 'PUT') return options.body;
        return multiRuleSettings;
      }
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth;
      if (path === '/prices/preview') return initialPreview;
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // Expand rules details
    const rulesSummary = screen.getByText(/条件匹配规则（高级/);
    fireEvent.click(rulesSummary);

    // Rule-2 Move Up button
    const moveUpBtns = screen.getAllByTestId('rule-up');
    // For rule 1, move up is disabled; for rule 2, move up is enabled
    expect((moveUpBtns[0] as HTMLButtonElement).disabled).toBe(true);
    expect((moveUpBtns[1] as HTMLButtonElement).disabled).toBe(false);

    // Move rule 2 up
    fireEvent.click(moveUpBtns[1]);

    // Save Pricing
    const savePricingBtn = screen.getByTestId('settings-pricing-save');
    fireEvent.click(savePricingBtn);

    await waitFor(() => {
      const putCalls = (mockApi.request as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call) => call[0] === '/settings' && call[1]?.method === 'PUT'
      );
      expect(putCalls.length).toBe(1);
      const savedRules = (putCalls[0][1].body as Settings).pricing.rules;
      expect(savedRules[0].id).toBe('rule-2');
      expect(savedRules[1].id).toBe('rule-1');
    });
  });

  // Test load failure handling
  it('renders honest error notice and retry button when initial load fails', async () => {
    const mockRequest = vi.fn(async () => {
      throw new Error('Connection refused');
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('settings-error')).toBeDefined();
      expect(screen.getByText('Connection refused')).toBeDefined();
    });
  });
});
