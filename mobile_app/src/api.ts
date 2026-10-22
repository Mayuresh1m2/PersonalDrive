import AsyncStorage from "@react-native-async-storage/async-storage";
import * as SecureStore from "expo-secure-store";
import { Platform } from "react-native";
import { PDriveClient, type Tokens } from "@pdrive/client";

const SERVER_KEY = "pdrive.server";
const TOKENS_KEY = "pdrive.tokens";

export interface ServerInfo {
  url: string;
  name: string;
  caSha256?: string;
}

let current: PDriveClient | null = null;
let server: ServerInfo | null = null;
const listeners = new Set<() => void>();

export function onSessionChange(fn: () => void) {
  listeners.add(fn);
  return () => void listeners.delete(fn);
}

function build(url: string) {
  return new PDriveClient({
    baseUrl: url,
    deviceName: `${Platform.OS === "ios" ? "iPhone" : "Android"} (PersonalDrive app)`,
    platform: Platform.OS,
    // Tokens live in the Keychain / Android Keystore.
    tokens: {
      load: async () => {
        const v = await SecureStore.getItemAsync(TOKENS_KEY);
        return v ? (JSON.parse(v) as Tokens) : null;
      },
      save: async (t) => {
        if (t) await SecureStore.setItemAsync(TOKENS_KEY, JSON.stringify(t));
        else await SecureStore.deleteItemAsync(TOKENS_KEY);
      },
    },
    onSignedOut: () => listeners.forEach((fn) => fn()),
  });
}

/** The signed-in client, or null before pairing. */
export async function getClient(): Promise<PDriveClient | null> {
  if (current) return current;
  const v = await AsyncStorage.getItem(SERVER_KEY);
  if (!v) return null;
  server = JSON.parse(v) as ServerInfo;
  current = build(server.url);
  return (await current.isSignedIn()) ? current : null;
}

export function getServer() {
  return server;
}

export async function connect(info: ServerInfo): Promise<PDriveClient> {
  server = info;
  await AsyncStorage.setItem(SERVER_KEY, JSON.stringify(info));
  current = build(info.url);
  return current;
}

export async function signOut() {
  try {
    await current?.logout();
  } catch {
    /* already invalid */
  }
  await SecureStore.deleteItemAsync(TOKENS_KEY);
  await AsyncStorage.removeItem(SERVER_KEY);
  current = null;
  server = null;
  listeners.forEach((fn) => fn());
}
