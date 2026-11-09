// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import path from 'node:path'
import { app } from 'electron'
import type { LaunchItems, LoginItemSettings } from 'electron'
import { APP_HIDDEN_ARGUMENT, APP_ID } from '@/shared/constants/app'
import type { StartupSettings } from '@/shared/types/ipc-channels'
import { currentPlatform } from '@/shared/utils/platform'

/**
 * Whether this platform has an OS login item Electron can write.
 *
 * `setLoginItemSettings` / `getLoginItemSettings` are documented for darwin and
 * win32 only. On Linux autostart is a per-desktop `.desktop` file Electron does
 * not manage, so both calls are skipped there and Settings reports the toggles
 * as unsupported instead of appearing to work while doing nothing.
 */
export function isLoginItemSupported(): boolean {
    const platform = currentPlatform()
    return platform === 'win32' || platform === 'darwin'
}

/**
 * Arguments the login item passes to the app it starts.
 *
 * This is the whole "was I started at login?" signal: nothing else adds
 * {@link APP_HIDDEN_ARGUMENT}, so a launch by hand still opens the window even
 * while `startHidden` is on.
 */
function loginItemArgs(settings: StartupSettings): string[] {
    return settings.startHidden ? [APP_HIDDEN_ARGUMENT] : []
}

/**
 * Write the OS login item so it matches `settings`. No-op where unsupported.
 *
 * Windows: Electron writes the `HKCU\...\Run` entry itself, defaulting the
 * executable to `process.execPath` and the value name to the AppUserModelId, so
 * neither `path` nor `name` is passed — pinning them would freeze the entry on a
 * path an app update moves. `args` is the only field that must be supplied.
 *
 * macOS: a login item carries no arguments, so `openAsHidden` is the only way to
 * ask for a hidden start. It is deprecated and has no effect on macOS 13 and up,
 * where the app opens its window at login as if it had been started by hand —
 * a platform limitation, not a configuration error.
 *
 * `openAtLogin: false` removes the entry, so this is also the "turn it off" path
 * and is safe to call unconditionally.
 *
 * An unpackaged build writes nothing. There `process.execPath` is the Electron
 * binary inside `node_modules` and the AppUserModelId is that same path, so a
 * `npm start` on a machine where the installed app already launches at login
 * would add a second entry that starts a bare Electron at sign-in. The
 * preference is still persisted, and the installed build applies it.
 */
export function applyLoginItem(settings: StartupSettings): void {
    if (!isLoginItemSupported() || !app.isPackaged) return

    app.setLoginItemSettings({
        openAtLogin: settings.launchAtLogin,
        openAsHidden: settings.startHidden,
        args: loginItemArgs(settings)
    })
}

/**
 * What {@link reconcileLoginItem} does to the OS entry at launch.
 *
 * `opt-out` writes nothing: the user switched the entry off outside the app
 * (disabled it in Task Manager or Windows Settings, or in macOS System
 * Settings), so the OS already says no and the stored preference is the part
 * that is out of date. The caller stores `launchAtLogin: false`.
 */
export type LoginItemPlan =
    | { kind: 'keep' }
    | { kind: 'remove' }
    | { kind: 'opt-out' }
    | { kind: 'register' }

/** Only an `enabled` macOS item starts at login; without a status, `openAtLogin` decides. */
function isMacItemEnabled(current: LoginItemSettings): boolean {
    return current.status ? current.status === 'enabled' : current.openAtLogin
}

/** Windows paths are case-insensitive, and the Run value may keep its quotes. */
function normalizeWindowsPath(value: string): string {
    return path.win32.normalize(value.replace(/^"|"$/g, '')).toLowerCase()
}

function sameArgs(a: readonly string[], b: readonly string[]): boolean {
    return a.length === b.length && a.every((arg, index) => arg === b[index])
}

/**
 * The per-user Windows Run entries this app owns: under its value name (the
 * AppUserModelId) or pointing at this executable. The name is matched as well as
 * the path because an update that moves the executable leaves the old entry under
 * the same name, and whether the user disabled THAT entry is what matters.
 */
function ownedWindowsItems(launchItems: readonly LaunchItems[], execPath: string): LaunchItems[] {
    const exe = normalizeWindowsPath(execPath)
    return launchItems.filter(
        item =>
            item.scope === 'user' &&
            (item.name === APP_ID || normalizeWindowsPath(item.path) === exe)
    )
}

/**
 * Decide what the OS login item needs at launch, given what the OS reports.
 *
 * The OS state wins over the stored preference. The user can turn the entry off
 * outside the app, and re-registering it at the next launch undoes a choice they
 * made on purpose: until this, every launch called `setLoginItemSettings`, whose
 * Windows write defaults `enabled` to true and so cleared the Task Manager
 * "Disabled" flag, and PAIR switched its own startup back on when it was started
 * by hand (2026-11-09). So nothing is written when the entry already matches, and
 * an entry the user disabled is an `opt-out`, never re-armed.
 *
 * Windows:
 * - preference on, an entry of this app the user disabled: opt out.
 * - preference on, an enabled entry with this executable and these arguments: keep.
 * - preference on, entry missing or stale (other arguments, or an executable an
 *   update moved): register.
 * - preference off, any entry of this app: remove it.
 *
 * macOS: preference on and the item not `enabled` (switched off or removed in
 * System Settings) is an opt-out; preference off and enabled is a removal.
 *
 * Ported from the launch-at-login rework proposed upstream (NVIDIA PR #97).
 */
export function planLoginItem({
    settings,
    current,
    platform,
    execPath
}: {
    settings: StartupSettings
    current: LoginItemSettings
    platform: 'win32' | 'darwin'
    execPath: string
}): LoginItemPlan {
    if (platform === 'darwin') {
        const enabled = isMacItemEnabled(current)
        if (settings.launchAtLogin) return enabled ? { kind: 'keep' } : { kind: 'opt-out' }
        return enabled ? { kind: 'remove' } : { kind: 'keep' }
    }

    const owned = ownedWindowsItems(current.launchItems ?? [], execPath)
    if (!settings.launchAtLogin) {
        return current.openAtLogin || owned.length > 0 ? { kind: 'remove' } : { kind: 'keep' }
    }
    if (owned.some(item => !item.enabled)) return { kind: 'opt-out' }

    const wantedArgs = loginItemArgs(settings)
    const exe = normalizeWindowsPath(execPath)
    const registered =
        current.openAtLogin ||
        owned.some(
            item => normalizeWindowsPath(item.path) === exe && sameArgs(item.args, wantedArgs)
        )
    return registered ? { kind: 'keep' } : { kind: 'register' }
}

/**
 * Bring the OS login item in line with `settings` at launch, writing only when
 * the plan says so. Returns the plan's kind, or `skipped` where nothing is
 * managed (an unsupported OS, or an unpackaged build: see {@link applyLoginItem}).
 * On `opt-out` the caller stores `launchAtLogin: false`.
 */
export function reconcileLoginItem(settings: StartupSettings): LoginItemPlan['kind'] | 'skipped' {
    const platform = currentPlatform()
    if (!app.isPackaged || (platform !== 'win32' && platform !== 'darwin')) return 'skipped'
    const plan = planLoginItem({
        settings,
        current: app.getLoginItemSettings({ args: loginItemArgs(settings) }),
        platform,
        execPath: process.execPath
    })
    if (plan.kind === 'register' || plan.kind === 'remove') applyLoginItem(settings)
    return plan.kind
}

/**
 * The login item as the OS currently reports it, for diagnostics. `null` where
 * unsupported.
 *
 * `settings` must be the same values {@link applyLoginItem} was given: on
 * Windows `openAtLogin` is only true when the registered arguments match the
 * ones asked about, so reading with the wrong arguments reports a login item
 * that is in fact registered as missing.
 */
export function readLoginItem(settings: StartupSettings): Electron.LoginItemSettings | null {
    if (!isLoginItemSupported()) return null
    return app.getLoginItemSettings({ args: loginItemArgs(settings) })
}

/**
 * Whether a process started with `argv` should open the Overview window.
 *
 * Only the login item passes {@link APP_HIDDEN_ARGUMENT}, so its absence means a
 * person started the app and expects to see it. Kept pure and separate from the
 * `app.whenReady()` body so the startup decision is testable without Electron.
 */
export function shouldOpenOverviewOnStart(argv: readonly string[]): boolean {
    return !argv.includes(APP_HIDDEN_ARGUMENT)
}
