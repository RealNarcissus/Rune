// LuminaSearch Tauri v2 Shell
//
// Responsibilities:
// - Manage Go backend sidecar lifecycle (spawn/kill) — implemented in tauri-shell-setup feature
// - Register global shortcut (Ctrl+Space) to toggle window visibility — implemented in tauri-shell-setup feature
// - Window management (show/hide/focus/always-on-top)
// - No business logic — all search operations go through the Go HTTP API
//
// This scaffolding provides the minimal Tauri shell that compiles and launches
// an empty window. Full functionality is added in subsequent features.

use tauri::Manager;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_global_shortcut::Builder::new().build())
        .setup(|app| {
            // Get the main window handle and make it visible on launch
            let window = app.get_webview_window("main").unwrap();

            // Show the window on startup (for development/testing)
            // In production, the window starts hidden and Ctrl+Space shows it
            let _ = window.show();
            let _ = window.set_focus();

            // Sidecar and global shortcut registration will be added
            // in the tauri-shell-setup feature (Milestone 3)

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
