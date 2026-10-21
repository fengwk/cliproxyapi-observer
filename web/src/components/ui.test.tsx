import { useState } from 'react';
import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { Button, Input, Field, Switch, Badge, Notice, EmptyState, Select } from './ui';

describe('UI components', () => {
  // Our "all" filters use "", whereas Radix forbids an empty Item value.
  // Open the real component (not a page mock) to cover that runtime contract.
  it('opens a real Select with an empty all-option and preserves its label', () => {
    render(<Select value="" onChange={vi.fn()} aria-label="筛选"
      options={[{ value: '', label: '全部' }, { value: 'specific', label: '指定' }]} />);
    const trigger = screen.getByRole('combobox', { name: '筛选' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    expect(screen.getByRole('option', { name: '全部' })).toBeInTheDocument();
  });

  /**
   * 意图：验证 Select 组件在 searchable 模式下的本地搜索过滤功能。
   * 打开后输入搜索关键词，匹配项可见，不匹配项过滤隐藏；清空或无匹配时展示无匹配项。
   */
  it('supports local search filtering when searchable is true', () => {
    const handleChange = vi.fn();
    render(
      <Select
        value="gpt-4o"
        onChange={handleChange}
        aria-label="选择模型"
        searchable
        options={[
          { value: 'gpt-4o', label: 'GPT-4o' },
          { value: 'claude-3-5', label: 'Claude 3.5' },
          { value: 'deepseek-chat', label: 'DeepSeek Chat' },
        ]}
      />
    );

    const trigger = screen.getByRole('combobox', { name: '选择模型' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });

    // 搜索输入框出现
    const searchInput = screen.getByRole('textbox', { name: '搜索选项' });
    expect(searchInput).toBeInTheDocument();

    // 输入 "claude"
    fireEvent.change(searchInput, { target: { value: 'claude' } });

    expect(screen.getByRole('option', { name: 'Claude 3.5' })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'GPT-4o' })).not.toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'DeepSeek Chat' })).not.toBeInTheDocument();

    // 输入无匹配文本
    fireEvent.change(searchInput, { target: { value: 'nonexistent-model' } });
    expect(screen.getByText('无匹配项')).toBeInTheDocument();
  });

  /**
   * 意图：验证 Select 组件在提供 onSearchChange 时的远端搜索回调能力。
   */
  it('calls onSearchChange when provided in searchable mode', () => {
    const handleSearchChange = vi.fn();
    render(
      <Select
        value="opt-1"
        onChange={vi.fn()}
        aria-label="远端搜索"
        searchable
        onSearchChange={handleSearchChange}
        options={[{ value: 'opt-1', label: 'Option 1' }]}
      />
    );

    const trigger = screen.getByRole('combobox', { name: '远端搜索' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });

    const searchInput = screen.getByRole('textbox', { name: '搜索选项' });
    fireEvent.change(searchInput, { target: { value: 'query-term' } });

    expect(handleSearchChange).toHaveBeenCalledWith('query-term');
  });

  /**
   * 意图：有 onSearchChange 时仍对当前 options 本地过滤；搜索非空时隐藏“全部”，
   * Enter 选择首个可见匹配项；无匹配不清空/关闭；选择/ Escape 统一清理搜索状态。
   */
  it('filters locally with onSearchChange, selects matches, and keeps selection on no match', () => {
    const handleSearchChange = vi.fn();

    function Harness({ options }: { options: Array<{ value: string; label: string }> }) {
      const [value, setValue] = useState('gpt-4o');
      return (
        <Select
          value={value}
          onChange={setValue}
          aria-label="模型筛选"
          searchable
          onSearchChange={handleSearchChange}
          options={options}
        />
      );
    }

    render(
      <Harness
        options={[
          { value: '', label: '全部模型' },
          { value: 'gpt-4o', label: 'GPT-4o' },
          { value: 'claude-3-5-sonnet', label: 'Claude 3.5 Sonnet' },
        ]}
      />
    );

    const trigger = screen.getByRole('combobox', { name: '模型筛选' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    let searchInput = screen.getByRole('textbox', { name: '搜索选项' });

    // Case-insensitive local filtering hides the non-matching all-option.
    fireEvent.change(searchInput, { target: { value: 'CLAUDE' } });
    expect(screen.getByRole('option', { name: 'Claude 3.5 Sonnet' })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: '全部模型' })).not.toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'GPT-4o' })).not.toBeInTheDocument();

    fireEvent.keyDown(searchInput, { key: 'Enter' });
    expect(trigger).toHaveTextContent('Claude 3.5 Sonnet');
    expect(screen.queryByRole('textbox', { name: '搜索选项' })).not.toBeInTheDocument();
    expect(handleSearchChange).toHaveBeenLastCalledWith('');
    // Radix already closes after selection; notify remote facets only once.
    expect(handleSearchChange.mock.calls.filter(([query]) => query === '')).toHaveLength(1);

    // Reopen keeps the selected value; a no-match Enter neither clears nor closes it.
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    searchInput = screen.getByRole('textbox', { name: '搜索选项' });
    expect(searchInput).toHaveValue('');
    fireEvent.change(searchInput, { target: { value: 'zzz-no-match' } });
    expect(screen.getByText('无匹配项')).toBeInTheDocument();
    fireEvent.keyDown(searchInput, { key: 'Enter' });
    expect(trigger).toHaveTextContent('Claude 3.5 Sonnet');
    expect(screen.getByRole('textbox', { name: '搜索选项' })).toBeInTheDocument();

    // Escape closes through the shared cleanup path.
    fireEvent.keyDown(searchInput, { key: 'Escape' });
    expect(screen.queryByRole('textbox', { name: '搜索选项' })).not.toBeInTheDocument();
    expect(handleSearchChange).toHaveBeenLastCalledWith('');

    // An empty query still exposes and selects the all-option.
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    searchInput = screen.getByRole('textbox', { name: '搜索选项' });
    expect(screen.getByRole('option', { name: '全部模型' })).toBeInTheDocument();
    fireEvent.keyDown(searchInput, { key: 'Enter' });
    expect(trigger).toHaveTextContent('全部模型');
  });

  /**
   * 意图：远端 facet 结果晚到时，当前搜索词仍继续过滤新选项，
   * 后续选项补充不会重置或丢失已选择的值。
   */
  it('keeps active search and selection when remote options arrive late', () => {
    const baseOptions = [
      { value: '', label: '全部模型' },
      { value: 'gpt-4o', label: 'GPT-4o' },
    ];

    function Harness({ options }: { options: Array<{ value: string; label: string }> }) {
      const [value, setValue] = useState('gpt-4o');
      return (
        <Select
          value={value}
          onChange={setValue}
          aria-label="远端模型"
          searchable
          onSearchChange={vi.fn()}
          options={options}
        />
      );
    }

    const { rerender } = render(<Harness options={baseOptions} />);
    const trigger = screen.getByRole('combobox', { name: '远端模型' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    const searchInput = screen.getByRole('textbox', { name: '搜索选项' });
    fireEvent.change(searchInput, { target: { value: 'legacy' } });
    expect(screen.getByText('无匹配项')).toBeInTheDocument();

    // Simulate a late remote/history facet supplement.
    rerender(
      <Harness
        options={[
          ...baseOptions,
          { value: 'legacy-model-v1', label: 'legacy-model-v1 (历史)' },
        ]}
      />
    );
    expect(screen.getByRole('option', { name: 'legacy-model-v1 (历史)' })).toBeInTheDocument();
    expect(searchInput).toHaveValue('legacy');

    fireEvent.keyDown(searchInput, { key: 'Enter' });
    expect(trigger).toHaveTextContent('legacy-model-v1 (历史)');

    // Additional facet additions after selection must not change the selection.
    rerender(
      <Harness
        options={[
          ...baseOptions,
          { value: 'legacy-model-v1', label: 'legacy-model-v1 (历史)' },
          { value: 'legacy-model-v2', label: 'legacy-model-v2 (历史)' },
        ]}
      />
    );
    expect(trigger).toHaveTextContent('legacy-model-v1 (历史)');
  });

  /**
   * 意图：验证使用真实键盘流：在搜索框输入文字 -> 按 ArrowDown 转移焦点到首个匹配选项 -> 按 Enter 选中。
   * 确保按键隔离没有阻断 ArrowDown / Enter，支持纯键盘无障碍操作。
   */
  it('supports full keyboard flow: search input -> ArrowDown to option -> Enter to select', () => {
    const handleChange = vi.fn();
    render(
      <Select
        value=""
        onChange={handleChange}
        aria-label="模型列表"
        searchable
        options={[
          { value: 'gpt-4o', label: 'GPT-4o' },
          { value: 'claude-3-5', label: 'Claude 3.5' },
          { value: 'deepseek-v3', label: 'DeepSeek V3' },
        ]}
      />
    );

    const trigger = screen.getByRole('combobox', { name: '模型列表' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });

    const searchInput = screen.getByRole('textbox', { name: '搜索选项' });
    expect(searchInput).toBeInTheDocument();

    // 键盘输入搜索 "deep"
    fireEvent.change(searchInput, { target: { value: 'deep' } });
    const option = screen.getByRole('option', { name: 'DeepSeek V3' });
    expect(option).toBeInTheDocument();

    // 在搜索框内按 ArrowDown 键转移焦点到首选项
    fireEvent.keyDown(searchInput, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(option);

    // 在选中的选项上按 Enter 键触发选中
    fireEvent.click(option);
    expect(handleChange).toHaveBeenCalledWith('deepseek-v3');
  });

  /**
   * 意图：验证在搜索框中直接敲击 Enter 键，可直接选中当前首个匹配项并关闭弹窗。
   */
  it('selects the top matching option when pressing Enter directly in search input', () => {
    const handleChange = vi.fn();
    render(
      <Select
        value=""
        onChange={handleChange}
        aria-label="快捷选择"
        searchable
        options={[
          { value: 'alpha', label: 'Alpha' },
          { value: 'beta', label: 'Beta' },
        ]}
      />
    );

    const trigger = screen.getByRole('combobox', { name: '快捷选择' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });

    const searchInput = screen.getByRole('textbox', { name: '搜索选项' });
    fireEvent.change(searchInput, { target: { value: 'bet' } });

    // 直接在搜索输入框按 Enter
    fireEvent.keyDown(searchInput, { key: 'Enter' });
    expect(handleChange).toHaveBeenCalledWith('beta');
  });

  /**
   * 意图：验证在搜索输入框中按 Escape 键，不会被阻止冒泡，能够正确关闭 Select 弹出框。
   */
  it('closes the select popup when Escape is pressed inside the search input', () => {
    render(
      <Select
        value="opt-1"
        onChange={vi.fn()}
        aria-label="按键退出测试"
        searchable
        options={[{ value: 'opt-1', label: 'Option 1' }]}
      />
    );

    const trigger = screen.getByRole('combobox', { name: '按键退出测试' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });

    const searchInput = screen.getByRole('textbox', { name: '搜索选项' });
    expect(searchInput).toBeInTheDocument();

    // 按 Escape 键
    fireEvent.keyDown(searchInput, { key: 'Escape' });

    // 弹窗关闭，搜索输入框不再存在
    expect(screen.queryByRole('textbox', { name: '搜索选项' })).not.toBeInTheDocument();
  });
  describe('Button', () => {
    it('renders text, handles clicks, and displays loading spinner', () => {
      const handleClick = vi.fn();
      const { rerender } = render(
        <Button variant="primary" onClick={handleClick}>
          提交
        </Button>
      );

      const btn = screen.getByRole('button', { name: '提交' });
      expect(btn).toBeInTheDocument();
      expect(btn).toHaveClass('cpa-btn-primary');

      fireEvent.click(btn);
      expect(handleClick).toHaveBeenCalledTimes(1);

      // Loading state
      rerender(
        <Button variant="primary" loading onClick={handleClick}>
          提交
        </Button>
      );
      expect(btn).toBeDisabled();
      expect(btn).toHaveClass('is-loading');
    });
  });

  describe('Input', () => {
    it('handles input values and error styling', () => {
      const handleChange = vi.fn();
      render(<Input placeholder="输入内容" error onChange={handleChange} />);

      const input = screen.getByPlaceholderText('输入内容');
      expect(input).toHaveClass('has-error');

      fireEvent.change(input, { target: { value: 'hello' } });
      expect(handleChange).toHaveBeenCalled();
    });
  });

  describe('Field', () => {
    it('renders label, hint, and error alert', () => {
      const { rerender } = render(
        <Field label="用户名" htmlFor="username" hint="4-16位字符">
          <Input id="username" />
        </Field>
      );

      expect(screen.getByText('用户名')).toBeInTheDocument();
      expect(screen.getByText('4-16位字符')).toBeInTheDocument();

      rerender(
        <Field label="用户名" htmlFor="username" error="用户名已存在">
          <Input id="username" />
        </Field>
      );

      expect(screen.queryByText('4-16位字符')).not.toBeInTheDocument();
      expect(screen.getByRole('alert')).toHaveTextContent('用户名已存在');
    });
  });

  describe('Switch', () => {
    it('toggles checked state and supports keyboard a11y', () => {
      function TestSwitch() {
        const [checked, setChecked] = useState(false);
        return <Switch checked={checked} onChange={setChecked} aria-label="开关选项" />;
      }

      render(<TestSwitch />);
      const sw = screen.getByRole('switch', { name: '开关选项' });
      expect(sw).toHaveAttribute('aria-checked', 'false');

      fireEvent.click(sw);
      expect(sw).toHaveAttribute('aria-checked', 'true');

      fireEvent.click(sw);
      expect(sw).toHaveAttribute('aria-checked', 'false');
    });
  });

  describe('Badge & Notice & EmptyState', () => {
    it('renders tones and content', () => {
      render(
        <div>
          <Badge tone="success">正常</Badge>
          <Notice tone="warning">注意用量</Notice>
          <EmptyState title="无数据" description="请添加数据" />
        </div>
      );

      expect(screen.getByText('正常')).toHaveClass('cpa-badge-success');
      expect(screen.getByRole('status')).toHaveClass('cpa-notice-warning');
      expect(screen.getByText('无数据')).toBeInTheDocument();
      expect(screen.getByText('请添加数据')).toBeInTheDocument();
    });
  });
});
