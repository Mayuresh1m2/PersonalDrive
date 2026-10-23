import * as BackgroundTask from "expo-background-task";
import * as TaskManager from "expo-task-manager";
import { registerRootComponent } from "expo";

import App from "./src/App";
import { runBackup } from "./src/cameraRoll";

import { BACKUP_TASK } from "./src/tasks";

// Background runs are short (the OS gives ~30s), so each run uploads what it
// can and the queue picks up where it left off next time.
TaskManager.defineTask(BACKUP_TASK, async () => {
  try {
    await runBackup({ deadlineMs: Date.now() + 25_000 });
    return BackgroundTask.BackgroundTaskResult.Success;
  } catch {
    return BackgroundTask.BackgroundTaskResult.Failed;
  }
});

registerRootComponent(App);
