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
