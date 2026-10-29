import { test, expect } from '@playwright/test';
import { ADMIN, adminToken, signIn } from './support';

test('open a cohort, enroll a learner, see the access pass, withdraw it', async ({ page }) => {
  const run = Date.now().toString(36).toUpperCase();
  const code = `E2E-${run}`;
  const reference = `LRN-${run}`;

  await signIn(page, ADMIN);
  await page.getByLabel('Cohort code').fill(code);
  await page.getByLabel('Seat capacity').fill('8');
  await page.getByRole('button', { name: 'Open cohort' }).click();

  const remaining = page.getByTestId('selected-remaining');
  await expect(page.getByRole('heading', { name: `Enroll in ${code}` })).toBeVisible();
  await expect(remaining).toHaveText('8');

  await page.getByLabel('Learner reference').fill(reference);
  await page.getByLabel('Seats', { exact: true }).fill('3');
  await page.getByRole('button', { name: 'Enroll' }).click();

  const row = page.locator(`tr[data-reference="${reference}"]`);
  await expect(row.getByTestId('status')).toHaveText('Enrolled', { timeout: 20_000 });
  await expect(row.getByTestId('pass')).toHaveText(/AP-[0-9A-F]{12}/);
  await expect(remaining).toHaveText('5');

  // Nothing is kept in the browser: a fresh load reads the same state from the API.
  // Nothing sensitive is stored either: the refresh cookie is HttpOnly, so a reload signs back in through it.
  expect(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }))).toBe(
    '{}',
  );
  await page.reload();
  await expect(page.getByTestId('whoami')).toContainText(ADMIN.email);
  await page.getByRole('button', { name: new RegExp(`^${code}`) }).click();
  const reloaded = page.locator(`tr[data-reference="${reference}"]`);
  await expect(reloaded.getByTestId('status')).toHaveText('Enrolled');
  await expect(page.getByTestId('selected-remaining')).toHaveText('5');

  await reloaded.getByRole('button', { name: 'Withdraw' }).click();
  await expect(reloaded.getByTestId('status')).toHaveText('Withdrawn');
  await expect(page.getByTestId('selected-remaining')).toHaveText('8');
  // The issued pass is not rolled back.
  await expect(reloaded.getByTestId('pass')).toHaveText(/AP-[0-9A-F]{12}/);
  const headers = { Authorization: `Bearer ${await adminToken(page.request)}` };
  const enrollments = await page.request.get(`/api/enrollments?cohortCode=${code}`, { headers });
  const [enrollment] = await enrollments.json();
  const pass = await page.request.get(`/api/passes/${enrollment.id}`, { headers });
  expect((await pass.json()).status).toBe('Issued');
});
