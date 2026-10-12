// Typed API calls. The types come from the Go side's OpenAPI document (`make gen-api` writes
// schema.d.ts), so a changed response shape breaks the typecheck instead of the page.
import type { components } from './schema'
import { del, get, post, put, qs } from './client'
import { enc } from '../format'

export * from './client'

type S = components['schemas']
export type Info = S['Info']
export type Stack = S['Stack']
export type Service = S['Service']
export type Container = S['DockerContainer']
export type ContainerDetails = S['DockerDetails']
export type StackList = S['StackList']
export type StackDetail = S['StackDetail']
export type HostInfo = S['HostInfo']
export type HostStats = S['HoststatsHost']
export type ContainerStats = S['HoststatsContainer']
export type FeedItem = S['FeedItem']
export type Job = S['JobsJob']
export type JobList = S['JobList']
export type JobDetail = S['JobDetail']
export type StackAction = S['ActionBody']['action']
export type EditableFile = S['FilesFile']
export type FileContent = S['FileContent']
export type SaveResult = S['SaveResult']
export type Revision = S['FilesRevision']
export type ValidationError = S['FilesValidationError']
export type UpdatesSummary = S['UpdatesSummary']
export type ServiceUpdate = S['UpdatesServiceUpdate']
export type UpdateRun = S['UpdatesRun']
export type RollbackPoint = S['UpdatesRollbackPoint']
export type UpdateSettings = S['UpdatesSettings']
export type StackPolicy = S['UpdatesStackPolicy']
export type Hook = S['UpdatesHook']
export type UpdatesOverview = S['UpdatesOverview']
export type StackUpdates = S['StackUpdates']
export type NotificationsOverview = Omit<S['NotificationsOverview'], 'rules'> & { rules: Rule[] }
export type ChannelView = S['ChannelView']
// rules come with empty lists left out: the UI always works with complete ones
export type Rule = Omit<S['NotifyRule'], 'id' | 'events' | 'stacks' | 'dedupe_minutes'> & { id: number; events: string[]; stacks: string[]; dedupe_minutes: number }
const normRule = (r: S['NotifyRule']): Rule => ({ ...r, id: r.id ?? 0, events: r.events ?? [], stacks: r.stacks ?? [], dedupe_minutes: r.dedupe_minutes ?? 0 })
export type EventType = S['NotifyEventType']
export type DeliveryLog = S['LogEntry']
export type HostSample = S['HostSample']
export type Backups = S['Backups']
export type Snapshot = S['Snapshot']
export type FolderListing = S['FilesListing']
export type FolderEntry = S['FilesEntry']
export type AnyFile = S['AnyFile']
export type PrunePreview = S['PrunePreview']
export type AuditEntry = S['StoreAuditEntry']
export type LoginEvent = S['StoreAuthEvent']
export type ServerSettings = S['ServerSettings']
export interface ChannelBody {
  type: 'ntfy' | 'gotify' | 'telegram' | 'webhook'
  name: string
  enabled: boolean
  config: Record<string, any>
}

export interface Session {
  authenticated: boolean
  confirmed: boolean
  read_only: boolean
}

export interface LogLine {
  c: string // container
  s: string // service
  t: string // timestamp
  e?: boolean // stderr
  m: string
}

export const api = {
  session: () => get<Session>('/api/session'),
  login: (password: string) => post<void>('/api/login', { password }),
  logout: () => post<void>('/api/logout'),
  confirm: (password: string) => post<void>('/api/confirm', { password }),

  info: () => get<Info>('/api/info'),
  host: () => get<HostInfo>('/api/host'),
  feed: () => get<{ items: FeedItem[] }>('/api/feed'),

  stacks: () => get<StackList>('/api/stacks'),
  stack: (id: string) => get<StackDetail>(`/api/stacks/${enc(id)}`),
  rescan: () => post<StackList>('/api/stacks/rescan'),
  stackAction: (id: string, action: StackAction, services?: string[]) =>
    post<Job>(`/api/stacks/${enc(id)}/actions`, { action, services: services?.length ? services : undefined }),
  containerAction: (id: string, action: 'start' | 'stop' | 'restart' | 'pause' | 'unpause') =>
    post<void>(`/api/containers/${enc(id)}/actions`, { action }),

  files: (id: string) => get<{ files: EditableFile[] }>(`/api/stacks/${enc(id)}/files`),
  file: (id: string, name: string) => get<FileContent>(`/api/stacks/${enc(id)}/files/${enc(name)}`),
  validate: (id: string, name: string, content: string) =>
    post<{ ok: boolean; error?: ValidationError }>(`/api/stacks/${enc(id)}/files/${enc(name)}/validate`, { content }),
  save: (id: string, name: string, content: string, base: string, apply = false) =>
    put<SaveResult>(`/api/stacks/${enc(id)}/files/${enc(name)}`, { content, base_sha256: base, apply }),
  revisions: (id: string, file?: string) => get<{ revisions: Revision[] }>(`/api/stacks/${enc(id)}/revisions${qs({ file })}`),
  revision: (id: string, rev: number) => get<Revision>(`/api/stacks/${enc(id)}/revisions/${rev}`),
  restore: (id: string, rev: number, apply = false) => post<SaveResult>(`/api/stacks/${enc(id)}/revisions/${rev}/restore`, { apply }),

  updates: () => get<UpdatesOverview>('/api/updates'),
  checkUpdates: (stack?: string) => post<Job>('/api/updates/check', { stack }),
  updateSettings: () => get<UpdateSettings>('/api/updates/settings'),
  saveUpdateSettings: (s: UpdateSettings) => put<UpdateSettings>('/api/updates/settings', s),
  stackUpdates: (id: string) => get<StackUpdates>(`/api/stacks/${enc(id)}/updates`),
  savePolicy: (id: string, p: StackPolicy) => put<StackUpdates>(`/api/stacks/${enc(id)}/policy`, p),
  rollback: (id: string, point: number) => post<Job>(`/api/stacks/${enc(id)}/rollbacks/${point}`),

  notifications: () => get<S['NotificationsOverview']>('/api/notifications').then((o) => ({ ...o, rules: (o.rules ?? []).map(normRule) }) as NotificationsOverview),
  createChannel: (c: ChannelBody) => post<ChannelView>('/api/notifications/channels', c),
  updateChannel: (id: number, c: ChannelBody) => put<ChannelView>(`/api/notifications/channels/${id}`, c),
  deleteChannel: (id: number) => del<void>(`/api/notifications/channels/${id}`),
  testChannel: (id: number) => post<{ ok: boolean; error?: string }>(`/api/notifications/channels/${id}/test`),
  createRule: (r: Rule) => post<Rule>('/api/notifications/rules', r),
  updateRule: (id: number, r: Rule) => put<Rule>(`/api/notifications/rules/${id}`, r),
  deleteRule: (id: number) => del<void>(`/api/notifications/rules/${id}`),

  hostHistory: () => get<{ samples: HostSample[] }>('/api/host/history'),
  prunePreview: () => get<PrunePreview>('/api/system/prune'),
  prune: (what: 'dangling' | 'unused' | 'build_cache') => post<Job>('/api/system/prune', { what }),
  audit: (p: { before?: number; limit?: number; q?: string } = {}) => get<{ entries: AuditEntry[] }>(`/api/audit${qs(p)}`),
  logins: () => get<{ events: LoginEvent[] }>('/api/logins'),
  serverSettings: () => get<ServerSettings>('/api/settings/server'),

  tree: (id: string, path = '') => get<FolderListing>(`/api/stacks/${enc(id)}/tree${qs({ path })}`),
  readAny: (id: string, path: string) => get<AnyFile>(`/api/stacks/${enc(id)}/file${qs({ path })}`),
  saveAny: (id: string, path: string, content: string, base: string, apply = false) =>
    put<SaveResult>(`/api/stacks/${enc(id)}/file`, { path, content, base_sha256: base, apply }),
  deleteAny: (id: string, path: string) => del<void>(`/api/stacks/${enc(id)}/file${qs({ path })}`),
  createFolder: (id: string, path: string) => post<void>(`/api/stacks/${enc(id)}/folder`, { path }),

  appFolders: () => get<{ root: string; depth: number; folders: string[] }>('/api/apps/folders'),
  validateApp: (b: { path: string; compose: string; env?: string }) => post<{ ok: boolean; error?: ValidationError }>('/api/apps/validate', b),
  createApp: (b: { path: string; compose: string; env?: string; start?: boolean }) => post<{ stack: string; job?: Job }>('/api/apps', b),

  backups: (id: string) => get<Backups>(`/api/stacks/${enc(id)}/backups`),
  takeSnapshot: (id: string, o: { stop: boolean; binds: boolean; keep?: number }) => post<Job>(`/api/stacks/${enc(id)}/snapshots`, o),
  restoreSnapshot: (id: string, sid: string) => post<Job>(`/api/stacks/${enc(id)}/snapshots/${sid}/restore`),
  deleteSnapshot: (id: string, sid: string) => del<void>(`/api/stacks/${enc(id)}/snapshots/${sid}`),

  jobs: (p: { stack?: string; before?: number; limit?: number } = {}) => get<JobList>(`/api/jobs${qs(p)}`),
  job: (id: number) => get<JobDetail>(`/api/jobs/${id}`),
  cancelJob: (id: number) => post<void>(`/api/jobs/${id}/cancel`),
}

export const urls = {
  logs: (p: { stack?: string; container?: string; services?: string; tail?: string | number; since?: string; follow?: number }) => `/api/logs${qs(p)}`,
  logsDownload: (p: { stack?: string; container?: string; services?: string; tail?: string }) => `/api/logs/download${qs(p)}`,
  jobStream: (id: number) => `/api/jobs/${id}/stream`,
  download: (id: string, path: string) => `/api/stacks/${enc(id)}/download${qs({ path })}`,
  upload: (id: string, path: string) => `/api/stacks/${enc(id)}/upload${qs({ path })}`,
  backup: (id: string, p: { snapshot?: string; file?: string; dump?: string }) => `/api/stacks/${enc(id)}/backups/download${qs(p)}`,
}

export { del, put }
