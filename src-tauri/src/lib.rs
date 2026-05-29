// Rune Tauri v2 Shell
//
// Responsibilities:
// - Manage Go backend sidecar lifecycle (spawn/kill)
// - Register global shortcut (Ctrl+Space) to toggle window visibility
// - Window management (show/hide/focus/always-on-top)
// - Focus loss (blur) window auto-hiding hook
// - Expose custom native command open_file executing xdg-open

use tauri::Manager;
use tauri_plugin_shell::ShellExt;
use tauri_plugin_global_shortcut::{GlobalShortcutExt, Shortcut};

#[tauri::command]
fn open_file(path: String) -> Result<(), String> {
    println!("rune: opening path natively: {}", path);
    std::process::Command::new("xdg-open")
        .arg(&path)
        .spawn()
        .map_err(|e| format!("Failed to invoke xdg-open for '{}': {}", path, e))?;
    Ok(())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    // 1. Build the global shortcut "Ctrl+Space" to toggle the search overlay window
    let ctrl_space = "Ctrl+Space".parse::<Shortcut>().unwrap();
    let shortcut_plugin = tauri_plugin_global_shortcut::Builder::new()
        .with_handler(move |app: &tauri::AppHandle, shortcut: &tauri_plugin_global_shortcut::Shortcut, event: tauri_plugin_global_shortcut::ShortcutEvent| {
            if shortcut == &ctrl_space && event.state() == tauri_plugin_global_shortcut::ShortcutState::Pressed {
                if let Some(window) = app.get_webview_window("main") {
                    let is_visible = window.is_visible().unwrap_or(false);
                    if is_visible {
                        let _ = window.hide();
                    } else {
                        let _ = window.show();
                        let _ = window.center();
                        let _ = window.set_focus();
                    }
                }
            }
        })
        .build();

    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(shortcut_plugin)
        .invoke_handler(tauri::generate_handler![open_file])
        .setup(move |app| {
            // Get window handle
            let window = app.get_webview_window("main").unwrap();

            // Set window decorations to false
            let _ = window.set_decorations(false);

            // Show, center, and focus the window on launch for E2E testing
            let _ = window.show();
            let _ = window.center();
            let _ = window.set_focus();
            
            // Set focus loss auto-hiding hook
            let w_handle = window.clone();
            window.on_window_event(move |event| {
                if let tauri::WindowEvent::Focused(false) = event {
                    let _ = w_handle.hide();
                }
            });

            // Start Go Daemon Sidecar Process
            log_sidecar_startup(app);

            // Register shortcut in global list
            let _ = app.global_shortcut().register(ctrl_space);

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}

fn log_sidecar_startup(app: &tauri::App) {
    let sidecar = app.shell().sidecar("runed").unwrap();
    let (mut rx, _child) = sidecar.spawn().unwrap();

    tauri::async_runtime::spawn(async move {
        use tauri_plugin_shell::process::CommandEvent;
        while let Some(event) = rx.recv().await {
            match event {
                CommandEvent::Stdout(line) => {
                    let s = String::from_utf8_lossy(&line);
                    print!("{}", s);
                }
                CommandEvent::Stderr(line) => {
                    let s = String::from_utf8_lossy(&line);
                    eprint!("{}", s);
                }
                _ => {}
            }
        }
    });
}
