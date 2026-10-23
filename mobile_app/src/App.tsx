import { useCallback, useEffect, useState } from "react";
import { ActivityIndicator, Alert, AppState, FlatList, Linking, Modal, Pressable, ScrollView, StyleSheet, Switch, Text, TextInput, View, useColorScheme } from "react-native";
import { CameraView, useCameraPermissions } from "expo-camera";
import * as BackgroundTask from "expo-background-task";
import { Image } from "expo-image";
import { StatusBar } from "expo-status-bar";
import { SafeAreaProvider, SafeAreaView } from "react-native-safe-area-context";
import { PDriveClient, findReachableUrl, parsePairing, type FileEntry, type Space } from "@pdrive/client";

import { connect, getClient, getServer, onSessionChange, signOut } from "./api";
import { freeUpSpace, loadSettings, runBackup, saveSettings, type BackupStatus, type Settings } from "./cameraRoll";

import { BACKUP_TASK } from "./tasks";

function useTheme() {
  const dark = useColorScheme() === "dark";
  return {
    bg: dark ? "#111317" : "#f7f7f8",
    card: dark ? "#181b20" : "#ffffff",
    text: dark ? "#e8eaee" : "#16181d",
    muted: dark ? "#9aa0ab" : "#666b75",
    border: dark ? "#2b3038" : "#e3e5e9",
    accent: dark ? "#4f8cff" : "#2563eb",
    danger: dark ? "#ff6b63" : "#c2302b",
    dark,
  };
}
type Theme = ReturnType<typeof useTheme>;

function Button({ title, onPress, primary, danger, disabled, t }: { title: string; onPress: () => void; primary?: boolean; danger?: boolean; disabled?: boolean; t: Theme }) {
  return (
    <Pressable
      onPress={onPress}
      disabled={disabled}
      style={({ pressed }) => [
        styles.button,
        { backgroundColor: primary ? t.accent : t.card, borderColor: primary ? t.accent : t.border, opacity: disabled ? 0.5 : pressed ? 0.8 : 1 },
      ]}
    >
      <Text style={{ color: primary ? "#fff" : danger ? t.danger : t.text, fontWeight: "600" }}>{title}</Text>
    </Pressable>
  );
}

// ---- Pairing ------------------------------------------------------------------

function Pairing({ onDone, t }: { onDone: () => void; t: Theme }) {
  const [perm, requestPerm] = useCameraPermissions();
  const [scanning, setScanning] = useState(false);
  const [busy, setBusy] = useState(false);
  const [manual, setManual] = useState(false);
  const [url, setUrl] = useState("https://");
  const [user, setUser] = useState("");
  const [pass, setPass] = useState("");
  const [error, setError] = useState("");

  const pairWith = async (text: string) => {
    setScanning(false);
    setBusy(true);
    setError("");
    try {
      const p = parsePairing(text);
      const reachable = await findReachableUrl(p.urls, { caSha256: p.ca_sha256 });
      if (!reachable) {
        throw new Error(
          "Can't reach the server. Make sure you're on the home Wi-Fi and that this phone trusts the server's certificate (see Help).",
        );
      }
      const client = await connect({ url: reachable, name: p.name ?? "PersonalDrive", caSha256: p.ca_sha256 });
      await client.pair(p.token);
      onDone();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const login = async () => {
    setBusy(true);
    setError("");
    try {
      const client = await connect({ url: url.replace(/\/+$/, ""), name: "PersonalDrive" });
      await client.login(user, pass);
      onDone();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  if (scanning) {
    return (
      <View style={{ flex: 1 }}>
        <CameraView style={{ flex: 1 }} barcodeScannerSettings={{ barcodeTypes: ["qr"] }} onBarcodeScanned={({ data }) => void pairWith(data)} />
        <View style={{ padding: 16 }}>
          <Button t={t} title="Cancel" onPress={() => setScanning(false)} />
        </View>
      </View>
    );
  }
  return (
    <ScrollView contentContainerStyle={{ padding: 24, gap: 16 }}>
      <Text style={[styles.h1, { color: t.text }]}>PersonalDrive</Text>
      <Text style={{ color: t.muted, fontSize: 16 }}>
        Back up your photos to your own server at home. On a computer, open the PersonalDrive web app → Devices →
        "Show pairing code", then scan it here.
      </Text>
      {busy ? <ActivityIndicator /> : (
        <Button t={t} primary title="Scan pairing code" onPress={async () => {
          if (!perm?.granted && !(await requestPerm()).granted) return setError("Camera access is needed to scan the code.");
          setScanning(true);
        }} />
      )}
      {error ? <Text style={{ color: t.danger }}>{error}</Text> : null}
      <Pressable onPress={() => setManual(!manual)}>
        <Text style={{ color: t.accent }}>{manual ? "Hide" : "Sign in with a password instead"}</Text>
      </Pressable>
      {manual && (
        <View style={{ gap: 10 }}>
          {[["Server address", url, setUrl, false], ["Username", user, setUser, false], ["Password", pass, setPass, true]].map(([label, value, set, secure]) => (
            <TextInput
              key={label as string}
              placeholder={label as string}
              placeholderTextColor={t.muted}
              value={value as string}
              onChangeText={set as (s: string) => void}
              secureTextEntry={secure as boolean}
              autoCapitalize="none"
              autoCorrect={false}
              style={[styles.input, { color: t.text, borderColor: t.border, backgroundColor: t.card }]}
            />
          ))}
          <Button t={t} title="Sign in" onPress={login} disabled={busy} />
        </View>
      )}
      <Pressable onPress={() => Linking.openURL("https://github.com/Mayuresh1m2/PersonalDrive/blob/main/mobile_app/README.md#trusting-your-server")}>
        <Text style={{ color: t.muted }}>Help: trusting your server's certificate</Text>
      </Pressable>
    </ScrollView>
  );
}

// ---- Backup tab -------------------------------------------------------------------

function BackupTab({ client, t }: { client: PDriveClient; t: Theme }) {
  const [settings, setSettings] = useState<Settings | null>(null);
  const [status, setStatus] = useState<BackupStatus | null>(null);
  const [spaces, setSpaces] = useState<Space[]>([]);
  const [running, setRunning] = useState(false);
  const [freeAllowed, setFreeAllowed] = useState(false);

  useEffect(() => {
    void loadSettings().then(setSettings);
    void client.serverInfo().then((i) => setFreeAllowed(!!i.safety?.free_up_space), () => {});
    void client.spaces().then((s) => setSpaces(s.filter((x) => x.permission === "write")), () => {});
  }, [client]);

  const run = useCallback(async () => {
    setRunning(true);
    try {
      setStatus(await runBackup({ onProgress: setStatus }));
    } finally {
      setRunning(false);
    }
  }, []);

  // Back up whenever the app comes to the foreground.
  useEffect(() => {
    if (!settings?.enabled) return;
    void run();
    const sub = AppState.addEventListener("change", (s) => s === "active" && void run());
    return () => sub.remove();
  }, [settings?.enabled, run]);

  const update = async (patch: Partial<Settings>) => {
    const next = { ...settings!, ...patch };
    setSettings(next);
    await saveSettings(next);
    if (patch.enabled === true) {
      await BackgroundTask.registerTaskAsync(BACKUP_TASK, { minimumInterval: 60 });
    } else if (patch.enabled === false) {
      await BackgroundTask.unregisterTaskAsync(BACKUP_TASK).catch(() => {});
    }
  };

  if (!settings) return <ActivityIndicator style={{ marginTop: 40 }} />;
  const row = (label: string, value: boolean, onChange: (v: boolean) => void, hint?: string) => (
    <View style={[styles.row, { borderColor: t.border }]}>
      <View style={{ flex: 1 }}>
        <Text style={{ color: t.text, fontSize: 16 }}>{label}</Text>
        {hint ? <Text style={{ color: t.muted, fontSize: 13 }}>{hint}</Text> : null}
      </View>
      <Switch value={value} onValueChange={onChange} />
    </View>
  );

  return (
    <ScrollView contentContainerStyle={{ padding: 16, gap: 12 }}>
      <View style={[styles.card, { backgroundColor: t.card, borderColor: t.border }]}>
        {row("Back up photos & videos", settings.enabled, (v) => {
          if (v && settings.since === 0) {
            Alert.alert("Which photos?", "Back up everything on this phone, or only new photos from now on?", [
              { text: "New only", onPress: () => void update({ enabled: true, since: Date.now() }) },
              { text: "Everything", onPress: () => void update({ enabled: true, since: 1 }) },
            ]);
          } else void update({ enabled: v });
        }, `Into ${settings.spaceId ? spaces.find((s) => s.id === settings.spaceId)?.name ?? "a shared space" : "your library"} / ${settings.root}`)}
        {row("Only on Wi-Fi", settings.wifiOnly, (v) => void update({ wifiOnly: v }), "The server is at home, so this is usually what you want")}
        {row("Only while charging", settings.chargingOnly, (v) => void update({ chargingOnly: v }))}
      </View>

      {spaces.length > 0 && (
        <View style={[styles.card, { backgroundColor: t.card, borderColor: t.border }]}>
          <Text style={{ color: t.muted, marginBottom: 6 }}>Destination</Text>
          {[{ id: undefined as number | undefined, name: "My library" }, ...spaces.map((s) => ({ id: s.id, name: s.name! }))].map((d) => (
            <Pressable key={d.name} onPress={() => void update({ spaceId: d.id })} style={{ paddingVertical: 8 }}>
              <Text style={{ color: settings.spaceId === d.id ? t.accent : t.text, fontSize: 16 }}>
                {settings.spaceId === d.id ? "● " : "○ "}{d.name}
              </Text>
            </Pressable>
          ))}
        </View>
      )}

      {settings.enabled && (
        <View style={[styles.card, { backgroundColor: t.card, borderColor: t.border, gap: 6 }]}>
          <Text style={{ color: t.text, fontSize: 16, fontWeight: "600" }}>
            {running ? "Backing up…" : status?.pending ? `${status.pending} waiting` : "Up to date"}
          </Text>
          {status && (
            <Text style={{ color: t.muted }}>
              {status.done} backed up this session · {status.pending} to go{status.failed ? ` · ${status.failed} failed` : ""}
              {status.waitingFor ? ` · ${status.waitingFor}` : ""}
            </Text>
          )}
          {status?.lastError ? <Text style={{ color: t.danger }}>{status.lastError}</Text> : null}
          <Button t={t} title="Back up now" onPress={run} disabled={running} />
        </View>
      )}

      {freeAllowed && (
        <View style={[styles.card, { backgroundColor: t.card, borderColor: t.border, gap: 8 }]}>
          <Text style={{ color: t.text, fontSize: 16, fontWeight: "600" }}>Free up space</Text>
          <Text style={{ color: t.muted }}>
            Removes photos older than 30 days from this phone, but only ones the server confirms it has and that are
            already in every backup (backup disk and offsite). They stay in your library. Your phone keeps them in
            "Recently deleted" for 30 more days.
          </Text>
          <Button t={t} title="Free up space" onPress={() =>
            Alert.alert("Remove backed-up photos from this phone?", "Only photos that are safely in every backup are removed.", [
              { text: "Cancel", style: "cancel" },
              {
                text: "Remove",
                style: "destructive",
                onPress: async () => {
                  try {
                    const n = await freeUpSpace(30);
                    Alert.alert(n ? `Removed ${n} items from this phone` : "Nothing is ready to remove yet");
                  } catch (e) {
                    Alert.alert("Nothing removed", (e as Error).message);
                  }
                },
              },
            ])
          } />
        </View>
      )}
    </ScrollView>
  );
}

// ---- Library tab ------------------------------------------------------------------

function LibraryTab({ client, t }: { client: PDriveClient; t: Theme }) {
  const [path, setPath] = useState("");
  const [items, setItems] = useState<FileEntry[]>([]);
  const [token, setToken] = useState<string | null>(null);
  const [viewing, setViewing] = useState<FileEntry | null>(null);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    try {
      setItems(await client.listAll(path));
      setToken(await client.accessToken());
      setError("");
    } catch (e) {
      setError((e as Error).message);
    }
  }, [client, path]);
  useEffect(() => void load(), [load]);

  const headers = token ? { Authorization: `Bearer ${token}` } : undefined;
  const isMedia = (f: FileEntry) => /^(image|video)\//.test(f.mime ?? "");
  return (
    <View style={{ flex: 1 }}>
      <View style={[styles.row, { borderColor: t.border, paddingHorizontal: 16 }]}>
        {path ? (
          <Pressable onPress={() => setPath(path.split("/").slice(0, -1).join("/"))}>
            <Text style={{ color: t.accent, fontSize: 16 }}>‹ Back</Text>
          </Pressable>
        ) : null}
        <Text style={{ color: t.text, fontSize: 16, fontWeight: "600", flex: 1, textAlign: path ? "center" : "left" }} numberOfLines={1}>
          {path || "My library"}
        </Text>
      </View>
      {error ? <Text style={{ color: t.danger, padding: 16 }}>{error}</Text> : null}
      <FlatList
        data={items}
        numColumns={3}
        keyExtractor={(f) => String(f.id)}
        onRefresh={load}
        refreshing={false}
        contentContainerStyle={{ padding: 4 }}
        renderItem={({ item: f }) => (
          <Pressable style={{ width: "33.33%", padding: 4 }} onPress={() => (f.is_dir ? setPath(f.path!) : isMedia(f) ? setViewing(f) : undefined)}>
            <View style={{ aspectRatio: 1, backgroundColor: t.card, borderRadius: 8, overflow: "hidden", alignItems: "center", justifyContent: "center", borderWidth: 1, borderColor: t.border }}>
              {isMedia(f) && headers ? (
                <Image source={{ uri: client.url(`/api/v1/files/${f.id}/thumbnail`, { size: 256 }), headers }} style={{ width: "100%", height: "100%" }} contentFit="cover" cachePolicy="disk" recyclingKey={f.sha256} />
              ) : (
                <Text style={{ fontSize: 30 }}>{f.is_dir ? "📁" : "📄"}</Text>
              )}
            </View>
            <Text numberOfLines={1} style={{ color: t.text, fontSize: 12, marginTop: 2 }}>{f.name}</Text>
          </Pressable>
        )}
      />
      <Modal visible={!!viewing} animationType="fade" onRequestClose={() => setViewing(null)}>
        <SafeAreaView style={{ flex: 1, backgroundColor: "black" }}>
          <Pressable onPress={() => setViewing(null)} style={{ padding: 16 }}>
            <Text style={{ color: "white", fontSize: 16 }}>Close</Text>
          </Pressable>
          {viewing && headers && (
            <Image source={{ uri: client.url(`/api/v1/files/${viewing.id}/thumbnail`, { size: 1024 }), headers }} style={{ flex: 1 }} contentFit="contain" />
          )}
        </SafeAreaView>
      </Modal>
    </View>
  );
}

// ---- Settings tab -------------------------------------------------------------------

function SettingsTab({ client, t }: { client: PDriveClient; t: Theme }) {
  const [me, setMe] = useState<{ user: { username?: string }; used_bytes: number } | null>(null);
  useEffect(() => void client.me().then(setMe as never, () => {}), [client]);
  const s = getServer();
  return (
    <ScrollView contentContainerStyle={{ padding: 16, gap: 12 }}>
      <View style={[styles.card, { backgroundColor: t.card, borderColor: t.border, gap: 4 }]}>
        <Text style={{ color: t.text, fontSize: 16, fontWeight: "600" }}>{s?.name}</Text>
        <Text style={{ color: t.muted }}>{s?.url}</Text>
        {me && <Text style={{ color: t.muted }}>Signed in as {me.user.username} · {(me.used_bytes / 2 ** 30).toFixed(1)} GB used</Text>}
      </View>
      <Button t={t} danger title="Sign out of this phone" onPress={() =>
        Alert.alert("Sign out?", "Backups stop until you pair again. Nothing is deleted.", [
          { text: "Cancel", style: "cancel" },
          { text: "Sign out", style: "destructive", onPress: () => void signOut() },
        ])
      } />
    </ScrollView>
  );
}

// ---- Shell -------------------------------------------------------------------------------

export default function App() {
  const t = useTheme();
  const [client, setClient] = useState<PDriveClient | null | undefined>(undefined);
  const [tab, setTab] = useState<"backup" | "library" | "settings">("backup");
  const refresh = useCallback(() => void getClient().then(setClient), []);
  useEffect(() => {
    refresh();
    return onSessionChange(refresh);
  }, [refresh]);

  return (
    <SafeAreaProvider>
      <SafeAreaView style={{ flex: 1, backgroundColor: t.bg }}>
        <StatusBar style={t.dark ? "light" : "dark"} />
        {client === undefined ? (
          <ActivityIndicator style={{ marginTop: 80 }} />
        ) : client === null ? (
          <Pairing t={t} onDone={refresh} />
        ) : (
          <>
            <View style={{ flex: 1 }}>
              {tab === "backup" && <BackupTab client={client} t={t} />}
              {tab === "library" && <LibraryTab client={client} t={t} />}
              {tab === "settings" && <SettingsTab client={client} t={t} />}
            </View>
            <View style={[styles.tabs, { borderColor: t.border, backgroundColor: t.card }]}>
              {(["backup", "library", "settings"] as const).map((k) => (
                <Pressable key={k} onPress={() => setTab(k)} style={styles.tab}>
                  <Text style={{ color: tab === k ? t.accent : t.muted, fontWeight: tab === k ? "700" : "500" }}>
                    {k === "backup" ? "Backup" : k === "library" ? "Library" : "Settings"}
                  </Text>
                </Pressable>
              ))}
            </View>
          </>
        )}
      </SafeAreaView>
    </SafeAreaProvider>
  );
}

const styles = StyleSheet.create({
  h1: { fontSize: 28, fontWeight: "700" },
  button: { borderWidth: 1, borderRadius: 10, paddingVertical: 12, paddingHorizontal: 16, alignItems: "center" },
  input: { borderWidth: 1, borderRadius: 10, padding: 12, fontSize: 16 },
  card: { borderWidth: 1, borderRadius: 12, padding: 14 },
  row: { flexDirection: "row", alignItems: "center", paddingVertical: 10, borderBottomWidth: StyleSheet.hairlineWidth, gap: 12 },
  tabs: { flexDirection: "row", borderTopWidth: 1 },
  tab: { flex: 1, alignItems: "center", paddingVertical: 14 },
});
