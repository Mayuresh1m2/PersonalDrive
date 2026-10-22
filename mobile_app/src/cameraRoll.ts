// Camera-roll backup: finds new photos/videos and uploads them with the
// shared persistent queue. Runs from the app and from the background task.
import AsyncStorage from "@react-native-async-storage/async-storage";
import * as Battery from "expo-battery";
import { File } from "expo-file-system";
import { Asset, AssetField, MediaType, Query, requestPermissionsAsync } from "expo-media-library";
import * as Network from "expo-network";
import { UploadQueue, type FileSource, type QueueItem, type QueueStorage } from "@pdrive/client";

import { getClient } from "./api";
import { allowed, freeable, pending, remotePathFor, type AssetMeta, type BackedUp, type Conditions } from "./backupPlan";

const DONE_KEY = "pdrive.backup.done";
const QUEUE_KEY = "pdrive.backup.queue";
const SETTINGS_KEY = "pdrive.backup.settings";

export interface Settings extends Conditions {
  enabled: boolean;
  spaceId?: number; // back up into a shared space instead of your own library
  root: string;
  /** Only photos taken after this (ms). Set when backup is first enabled, unless "all photos" was chosen. */
  since: number;
}

export const defaultSettings: Settings = { enabled: false, wifiOnly: true, chargingOnly: false, root: "Camera Roll", since: 0 };

export async function loadSettings(): Promise<Settings> {
  const v = await AsyncStorage.getItem(SETTINGS_KEY);
  return { ...defaultSettings, ...(v ? (JSON.parse(v) as Partial<Settings>) : {}) };
}

export async function saveSettings(s: Settings) {
  await AsyncStorage.setItem(SETTINGS_KEY, JSON.stringify(s));
}

async function loadDone(): Promise<Record<string, BackedUp>> {
  const v = await AsyncStorage.getItem(DONE_KEY);
  return v ? (JSON.parse(v) as Record<string, BackedUp>) : {};
}

const queueStorage: QueueStorage = {
  load: async () => {
    const v = await AsyncStorage.getItem(QUEUE_KEY);
    return v ? (JSON.parse(v) as QueueItem[]) : [];
  },
  save: (items) => AsyncStorage.setItem(QUEUE_KEY, JSON.stringify(items)),
};

/** Reads an asset's file in slices without loading it all into memory. */
async function assetSource(assetId: string): Promise<FileSource | null> {
  let uri: string;
  try {
    uri = await new Asset(assetId).getUri();
  } catch {
    return null; // deleted from the phone meanwhile
  }
  const file = new File(uri);
  if (!file.exists) return null;
  const size = file.size ?? 0;
  return {
    size,
    mtime: new Date(file.modificationTime ?? Date.now()),
    slice: async (start, end) => {
      const h = file.open();
      try {
        h.offset = start;
        return await h.readBytes(end - start);
      } finally {
        h.close();
      }
    },
  };
}

async function canRun(s: Settings): Promise<string | null> {
  const net = await Network.getNetworkStateAsync();
  const state = await Battery.getBatteryStateAsync();
  const charging = state === Battery.BatteryState.CHARGING || state === Battery.BatteryState.FULL;
  return allowed(s, { connected: !!net.isConnected, wifi: net.type === Network.NetworkStateType.WIFI }, charging);
}

async function listAssets(since: number): Promise<AssetMeta[]> {
  const out: AssetMeta[] = [];
  for (let offset = 0; ; offset += 500) {
    const page = await new Query()
      .within(AssetField.MEDIA_TYPE, [MediaType.IMAGE, MediaType.VIDEO])
      .gte(AssetField.CREATION_TIME, since)
      .orderBy({ key: AssetField.CREATION_TIME, ascending: true })
      .limit(500)
      .offset(offset)
      .exeForMetadata();
    out.push(...page.map((a) => ({ ...a, mediaType: String(a.mediaType) })));
    if (page.length < 500) return out;
  }
}

export interface BackupStatus {
  pending: number;
  uploading: number;
  failed: number;
  done: number;
  waitingFor: string | null;
  lastError?: string;
}

let running: Promise<BackupStatus> | null = null;

/**
 * Finds new assets, queues them, and uploads until done, out of time, or
 * conditions change. Safe to call concurrently (calls share one run).
 */
export function runBackup(opts: { deadlineMs?: number; onProgress?: (s: BackupStatus) => void } = {}): Promise<BackupStatus> {
  running ??= (async () => {
    try {
      return await backupOnce(opts);
    } finally {
      running = null;
    }
  })();
  return running;
}

async function backupOnce({ deadlineMs, onProgress }: { deadlineMs?: number; onProgress?: (s: BackupStatus) => void }) {
  const settings = await loadSettings();
  const client = await getClient();
  const status: BackupStatus = { pending: 0, uploading: 0, failed: 0, done: 0, waitingFor: null };
  if (!settings.enabled || !client) return status;
  const perm = await requestPermissionsAsync();
  if (!perm.granted) return { ...status, waitingFor: "photo access" };

  const done = await loadDone();
  const queue = new UploadQueue(client, {
    storage: queueStorage,
    open: (it) => assetSource(it.ref),
    concurrency: 1,
    canRun: async () => {
      if (deadlineMs && Date.now() > deadlineMs) return false;
      status.waitingFor = await canRun(settings);
      return status.waitingFor === null;
    },
    onItem: async (it) => {
      if (it.state === "done") {
        done[it.ref] = { path: it.path, sha256: it.sha256, modified: Date.now(), at: Date.now() };
        await AsyncStorage.setItem(DONE_KEY, JSON.stringify(done));
      }
      Object.assign(status, await queue.stats(), { lastError: it.lastError });
      onProgress?.({ ...status });
    },
  });

  // Queue anything new (skipping what's already queued).
  const queued = new Set((await queue.list()).filter((i) => i.state !== "done").map((i) => i.ref));
  for (const a of pending(await listAssets(settings.since), done)) {
    if (!queued.has(a.id)) await queue.add({ ref: a.id, path: remotePathFor(a, settings.root), spaceId: settings.spaceId });
  }
  await queue.clearDone();
  Object.assign(status, await queue.stats());
  onProgress?.({ ...status });
  await queue.run();
  return Object.assign(status, await queue.stats());
}

/**
 * Deletes photos from the phone that are safely on the server (verified by
 * hash) and older than keepDays. The OS asks the user to confirm.
 */
export async function freeUpSpace(keepDays = 30): Promise<number> {
  const client = await getClient();
  if (!client) return 0;
  const done = await loadDone();
  const assets = await listAssets(0);
  const hashes = [...new Set(Object.values(done).map((d) => d.sha256).filter((h): h is string => !!h))];
  const onServer = new Set<string>();
  for (let i = 0; i < hashes.length; i += 1000) {
    for (const h of await client.have(hashes.slice(i, i + 1000))) onServer.add(h);
  }
  const victims = freeable(assets, done, onServer, keepDays);
  if (victims.length > 0) await Asset.delete(victims.map((a) => new Asset(a.id)));
  return victims.length;
}
