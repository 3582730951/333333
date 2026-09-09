import { expect, test, type Page } from '@playwright/test';

async function openBalanceEditor(page: Page, failEgress = false) {
  await page.route('**/*', async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === '/auth/me') return route.fulfill({ json: { authed: true, role: 'admin', email: 'admin@example.test' } });
    if (path === '/healthz') return route.fulfill({ json: { ok: true } });
    if (!path.startsWith('/admin/')) return route.continue();
    if (path === '/admin/groups') return route.fulfill({ json: [{ name: 'pool-a', account_count: 2, active_account_count: 2 }] });
    if (path === '/admin/user-groups') return route.fulfill({ json: [{
      id: 'balance-group', name: 'Balanced group', targets: [{ kind: 'account_pool_group', id: 'pool-a' }],
      dynamic_pool_balance_enabled: true, dynamic_pool_balance_rpm_threshold: 10,
      egress_rpm_balance_enabled: true, egress_rpm_balance_threshold: 20,
      egress_rpm_balance_egress_ids: ['exit-a', 'exit-b'],
    }] });
    if (path === '/admin/egress-profiles') {
      if (failEgress) return route.fulfill({ status: 503, json: { error: { message: 'Outlet list unavailable' } } });
      return route.fulfill({ json: [{ id: 'exit-a', name: 'Exit A' }, { id: 'exit-b', name: 'Exit B' }] });
    }
    return route.fulfill({ json: [] });
  });
  await page.goto('/console/groups');
  await page.getByRole('tab', { name: '用户分组', exact: true }).click();
  await page.getByRole('button', { name: '用户分组操作', exact: true }).click();
  await page.getByRole('menuitem', { name: '编辑完整策略', exact: true }).click();
  return page.getByRole('dialog');
}

for (const width of [1440, 390]) {
  test(`balance switches and outlet order save independently at ${width}px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    const dialog = await openBalanceEditor(page);
    await expect(dialog.getByText('负载均衡只分配新会话')).toBeVisible();
    await expect(dialog.getByRole('switch', { name: '启用网络出口 RPM 均衡' })).toBeChecked();
    await dialog.getByRole('button', { name: '上移 Exit B', exact: true }).click();
    const rows = dialog.getByRole('list', { name: '参与均衡的出口顺序' }).getByRole('listitem');
    await expect(rows.first()).toContainText('Exit B');
    await dialog.getByRole('switch', { name: '启用账号池内动态均衡' }).click();
    await expect(dialog.getByRole('switch', { name: '启用网络出口 RPM 均衡' })).toBeChecked();
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await dialog.screenshot({ path: testInfo.outputPath(`balance-editor-${width}.png`) });

    const saved = page.waitForRequest((request) => request.method() === 'PUT' && new URL(request.url()).pathname === '/admin/user-groups/balance-group');
    await dialog.getByRole('button', { name: '保存用户分组', exact: true }).click();
    expect((await saved).postDataJSON()).toMatchObject({
      dynamic_pool_balance_enabled: false, dynamic_pool_balance_rpm_threshold: 10,
      egress_rpm_balance_enabled: true, egress_rpm_balance_threshold: 20,
      egress_rpm_balance_egress_ids: ['exit-b', 'exit-a'],
    });
  });
}

test('an outlet-list failure preserves saved selections and offers retry', async ({ page }) => {
  const dialog = await openBalanceEditor(page, true);
  await expect(dialog.getByText('出口列表加载失败', { exact: true })).toBeVisible();
  await expect(dialog.getByRole('button', { name: '重试', exact: true })).toBeVisible();
  const rows = dialog.getByRole('list', { name: '参与均衡的出口顺序' }).getByRole('listitem');
  await expect(rows.first()).toContainText('exit-a');
  await expect(rows.nth(1)).toContainText('exit-b');
  await expect(dialog.getByRole('button', { name: '上移 exit-b', exact: true })).toBeDisabled();
});
