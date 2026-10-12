import { rmSync } from 'node:fs'
import { expect, test, type Page } from '@playwright/test'

const PASSWORD = process.env.DM_E2E_PASSWORD ?? 'docker-manager-dev'

async function login(page: Page) {
  await page.goto('/')
  await page.getByPlaceholder('Enter your password').fill(PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.locator('aside').getByRole('link', { name: 'Dashboard' })).toBeVisible()
}

test.describe.configure({ mode: 'serial' })

test('wrong password is refused, the right one lets you in', async ({ page }) => {
  await page.goto('/')
  await page.getByPlaceholder('Enter your password').fill('not the password')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByText('wrong password')).toBeVisible()
  await login(page)
  await expect(page.locator('aside').getByText('live')).toBeVisible()
})

test('dashboard lists the fixture stacks with their state', async ({ page }) => {
  await login(page)
  const table = page.locator('table').first()
  await expect(table.getByRole('link', { name: 'worker', exact: true })).toBeVisible()
  await expect(table.getByRole('link', { name: 'group/built-app' })).toBeVisible()
  const broken = table.locator('tr', { has: page.getByRole('link', { name: 'broken', exact: true }) })
  await expect(broken.getByText('invalid')).toBeVisible()
  const cards = page.locator('main .card')
  for (const label of ['Docker', 'Updates', 'Host', 'Disk']) await expect(cards.getByText(label, { exact: true }).first()).toBeVisible()
})

test('stack page shows services and live logs', async ({ page }) => {
  await login(page)
  await page.goto('/stacks/worker')
  await expect(page.getByRole('heading', { name: 'worker' })).toBeVisible()
  await expect(page.getByText('busybox:1.36').first()).toBeVisible()
  await page.getByRole('tab', { name: 'Logs' }).click()
  await expect(page.getByText(/tick \d+ \(hello from \.env\)/).first()).toBeVisible()
  await page.getByPlaceholder('Filter (regex)…').fill('warn: every fifth')
  await expect(page.getByText(/warn: every fifth tick/).first()).toBeVisible()
})

test('restart runs as a job and finishes', async ({ page }) => {
  await login(page)
  await page.goto('/stacks/worker')
  await page.getByRole('button', { name: 'Restart' }).first().click()
  await expect(page.getByText(/Restart worker: succeeded/)).toBeVisible({ timeout: 45_000 })
})

test('compose editor validates, saves with a diff, and history restores', async ({ page }) => {
  await login(page)
  await page.goto('/stacks/worker?tab=compose')
  const editor = page.locator('.cm-content')
  await expect(editor).toContainText('busybox:1.36')
  await expect(page.getByText('✓ valid')).toBeVisible()

  // break it: the line is reported
  await editor.click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type('\n  oops: [unclosed')
  await expect(page.getByText(/✕ line \d+/)).toBeVisible()
  await page.getByRole('button', { name: 'Revert' }).click()
  await page.getByRole('button', { name: 'Discard' }).click()
  await expect(page.getByText('✓ valid')).toBeVisible()

  // a harmless change, saved through the diff dialog
  await editor.click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type('\n# e2e marker')
  await expect(page.getByText('✓ valid')).toBeVisible()
  await page.keyboard.press('Control+s')
  await expect(page.getByRole('dialog', { name: /Save docker-compose.yml/ })).toBeVisible()
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await expect(page.getByText('docker-compose.yml saved')).toBeVisible()

  // back to the version before the edit
  await page.getByRole('tab', { name: 'History' }).click()
  const edited = page.getByRole('button', { name: /docker-compose.yml edited here/ })
  await expect(edited).toBeVisible()
  await page.getByRole('button', { name: /docker-compose.yml first seen/ }).click()
  await page.getByRole('button', { name: 'Restore', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Restore', exact: true }).click()
  await expect(page.getByText('docker-compose.yml restored')).toBeVisible()
  await page.getByRole('tab', { name: 'Compose' }).click()
  await expect(page.locator('.cm-content')).not.toContainText('e2e marker')
})

test('.env opens from the Files tab after the password', async ({ page }) => {
  await login(page)
  await page.goto('/stacks/worker?tab=files')
  await expect(page.getByRole('tab', { name: '.env' })).toHaveCount(0)
  await page.getByRole('button', { name: /^\.env/ }).click()
  await page.getByRole('dialog', { name: 'Confirm your password' }).getByLabel('Password').fill(PASSWORD)
  await page.getByRole('button', { name: 'Confirm' }).click()
  await expect(page.locator('.cm-content')).toContainText('GREETING=hello from .env')
})

test('update check runs and the updates page lists every service', async ({ page }) => {
  await login(page)
  await page.goto('/updates')
  await page.getByRole('button', { name: 'Check now' }).click()
  await expect(page.getByText(/Check for updates: succeeded/)).toBeVisible({ timeout: 45_000 })
  const row = page.locator('tbody tr', { has: page.getByRole('link', { name: 'worker', exact: true }) })
  await expect(row).toContainText('busybox:1.36')
  await expect(row).toContainText('up to date')
})

test('a shell opens in a running container', async ({ page }) => {
  await login(page)
  await page.goto('/stacks/worker')
  await page.getByTitle('Open a shell').first().click()
  await page.getByRole('dialog', { name: 'Confirm your password' }).getByLabel('Password').fill(PASSWORD)
  await page.getByRole('button', { name: 'Confirm' }).click()
  await expect(page.getByText('connected')).toBeVisible()
  await page.keyboard.type('echo e2e-$((6*7))\n')
  await expect(page.locator('.xterm-rows')).toContainText('e2e-42')
})

test('phones get the stacked list and a drawer', async ({ browser }) => {
  const page = await browser.newPage({ viewport: { width: 390, height: 844 } })
  await login(page)
  await expect(page.getByRole('button', { name: 'Open menu' })).toBeVisible()
  await page.getByRole('button', { name: 'Open menu' }).click()
  await expect(page.locator('aside').getByRole('link', { name: 'Notifications' })).toBeVisible()
  await page.locator('aside').getByRole('link', { name: 'Updates' }).click()
  await expect(page).toHaveURL(/\/updates/)
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth)
  expect(overflow).toBeLessThanOrEqual(1)
})

test('files tab browses the stack folder and edits a config file', async ({ page }) => {
  await login(page)
  await page.goto('/stacks/web?tab=files')
  await page.getByRole('button', { name: 'conf', exact: true }).click()
  await expect(page.getByRole('button', { name: /logo\.png/ })).toBeVisible()
  await page.getByRole('button', { name: /settings\.json/ }).click()
  await page.getByRole('dialog', { name: 'Confirm your password' }).getByLabel('Password').fill(PASSWORD)
  await page.getByRole('button', { name: 'Confirm' }).click()
  const editor = page.locator('.cm-content')
  await expect(editor).toContainText('"greeting": "hello"')
  // broken JSON is refused with its line
  await editor.click()
  await page.keyboard.press('Control+End')
  await page.keyboard.type('oops')
  await page.keyboard.press('Control+s')
  await page.getByRole('button', { name: 'Save', exact: true }).click()
  await expect(page.getByText(/✕ line \d+: JSON/)).toBeVisible()
  await page.getByRole('button', { name: 'Revert' }).click()
  await page.getByRole('button', { name: 'Discard' }).click()
  // binary files offer a download instead
  await page.getByRole('button', { name: /logo\.png/ }).click()
  await expect(page.getByText(/binary file/)).toBeVisible()
})

test('a new app is created from a pasted compose file and shows up as a stack', async ({ page }) => {
  const name = `e2e-new-${Date.now()}`
  try {
    await login(page)
    await page.getByRole('link', { name: 'New app' }).first().click()
    await page.getByLabel('Folder').fill(name)
    await expect(page.getByRole('button', { name: /^Create/ })).toBeDisabled()
    await page.locator('.cm-content').first().click()
    await page.keyboard.insertText('services:\n  app:\n    image: busybox:1.36\n    command: sleep 3600\n')
    await expect(page.getByText('✓ valid')).toBeVisible()
    await page.getByLabel('Start it right away').uncheck()
    await page.getByRole('button', { name: 'Create', exact: true }).click()
    await expect(page).toHaveURL(new RegExp(`/stacks/${name}`))
    await expect(page.getByRole('heading', { name })).toBeVisible()
    await expect(page.getByText('down', { exact: true }).first()).toBeVisible()
  } finally {
    rmSync(`../dev/apps/${name}`, { recursive: true, force: true })
  }
})

test('the new-app generator fills in a compose file from an image', async ({ page }) => {
  await login(page)
  await page.goto('/new')
  await page.getByLabel('Folder').fill('gen-test')
  await page.getByRole('button', { name: 'Generate from an image…' }).click()
  await page.getByLabel('Image', { exact: true }).fill('busybox:1.36')
  await page.getByRole('button', { name: 'Fill in compose file' }).click()
  await expect(page.locator('.cm-content').first()).toContainText('gen-test:')
  await expect(page.getByText('✓ valid')).toBeVisible()
})

test('backups: snapshot a volume, restore it, delete it', async ({ page }) => {
  await login(page)
  await page.goto('/stacks/cache?tab=backups')
  await expect(page.getByText(/1 volume/)).toBeVisible()
  await page.getByRole('button', { name: 'Snapshot now' }).click()
  await expect(page.getByText(/Snapshot cache: succeeded/)).toBeVisible({ timeout: 45_000 })
  await expect(page.getByText('manual', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: 'Restore' }).first().click()
  await page.getByRole('dialog').getByRole('button', { name: 'Restore' }).click()
  await page.getByRole('dialog', { name: 'Confirm your password' }).getByLabel('Password').fill(PASSWORD)
  await page.getByRole('button', { name: 'Confirm' }).click()
  await expect(page.getByRole('dialog', { name: /Restore cache from/ }).getByText('succeeded')).toBeVisible({ timeout: 60_000 })
  await page.keyboard.press('Escape')
  await expect(page.getByText('before restore', { exact: true })).toBeVisible()
  for (let i = 0; i < 2; i++) {
    await page.getByRole('button', { name: 'Delete' }).first().click()
    await page.getByRole('dialog').getByRole('button', { name: 'Delete' }).click()
  }
  await expect(page.getByText('No snapshots yet.')).toBeVisible()
})
