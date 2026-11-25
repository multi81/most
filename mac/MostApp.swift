// «Мост» для macOS: оболочка на Swift вокруг ядра most-core (Go). Окно — WKWebView с тем же дизайном, что на
// Windows и Android; значок в строке меню; закрытие окна не выключает программу (каналы работают дальше).
import Cocoa
import WebKit

let rdpBundleID = "com.microsoft.rdc.macos"                              // Microsoft «Windows App»
let rdpStoreURL = "macappstore://apps.apple.com/app/id1295203466"

final class AppDelegate: NSObject, NSApplicationDelegate, WKScriptMessageHandler, WKUIDelegate, WKNavigationDelegate, WKDownloadDelegate, NSWindowDelegate {
    var window: NSWindow!
    var web: WKWebView!
    var core: Process?
    var status: NSStatusItem!
    var coreURL: String?
    var coreOut: Pipe?        // держим, чтобы канал вывода ядра не закрылся
    var quitting = false

    // --hidden — запуск при входе в систему: только значок в строке меню, окно по щелчку
    let hidden = CommandLine.arguments.contains("--hidden")

    func applicationDidFinishLaunching(_ n: Notification) {
        if activateOtherInstance() || moveToApplications() { return }
        makeMainMenu()
        startCore()
        makeWindow()
        makeStatusItem()
    }

    // Главное меню. Без меню «Правка» в macOS не работают ⌘V/⌘C/⌘X/⌘A/⌘Z в полях ввода (команды идут через него).
    func makeMainMenu() {
        let main = NSMenu()
        let appItem = NSMenuItem(); main.addItem(appItem)
        let appMenu = NSMenu()
        appMenu.addItem(NSMenuItem(title: "Скрыть «Мост»", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h"))
        appMenu.addItem(.separator())
        appMenu.addItem(NSMenuItem(title: "Выйти из «Моста»", action: #selector(quit), keyEquivalent: "q"))
        appItem.submenu = appMenu

        let editItem = NSMenuItem(); main.addItem(editItem)
        let edit = NSMenu(title: "Правка")
        edit.addItem(NSMenuItem(title: "Отменить", action: Selector(("undo:")), keyEquivalent: "z"))
        let redo = NSMenuItem(title: "Повторить", action: Selector(("redo:")), keyEquivalent: "z")
        redo.keyEquivalentModifierMask = [.command, .shift]
        edit.addItem(redo)
        edit.addItem(.separator())
        edit.addItem(NSMenuItem(title: "Вырезать", action: #selector(NSText.cut(_:)), keyEquivalent: "x"))
        edit.addItem(NSMenuItem(title: "Скопировать", action: #selector(NSText.copy(_:)), keyEquivalent: "c"))
        edit.addItem(NSMenuItem(title: "Вставить", action: #selector(NSText.paste(_:)), keyEquivalent: "v"))
        edit.addItem(NSMenuItem(title: "Удалить", action: #selector(NSText.delete(_:)), keyEquivalent: ""))
        edit.addItem(NSMenuItem(title: "Выбрать всё", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a"))
        editItem.submenu = edit

        let winItem = NSMenuItem(); main.addItem(winItem)
        let win = NSMenu(title: "Окно")
        win.addItem(NSMenuItem(title: "Свернуть", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m"))
        win.addItem(NSMenuItem(title: "Закрыть окно", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w"))
        winItem.submenu = win
        NSApp.windowsMenu = win

        NSApp.mainMenu = main
    }

    // уже запущен другой «Мост» (два ядра с одним ключом AWG сбивали бы друг друга) — показать его и выйти
    func activateOtherInstance() -> Bool {
        // --moved: нас открыла копия из Загрузок/сборки, она сейчас закрывается — её не считаем
        if CommandLine.arguments.contains("--moved") { return false }
        let me = NSRunningApplication.current.processIdentifier
        guard let other = NSRunningApplication.runningApplications(withBundleIdentifier: Bundle.main.bundleIdentifier ?? "")
            .first(where: { $0.processIdentifier != me }) else { return false }
        if !hidden { other.activate() }
        quitting = true
        NSApp.terminate(nil)
        return true
    }

    // первый запуск не из «Программ» (из Загрузок, архива, папки сборки) — скопировать себя туда и открыть оттуда
    func moveToApplications() -> Bool {
        let fm = FileManager.default
        let src = Bundle.main.bundlePath
        let home = NSHomeDirectory()
        if src.hasPrefix("/Applications/") || src.hasPrefix(home + "/Applications/") { return false }
        var dir = "/Applications"
        if !fm.isWritableFile(atPath: dir) {
            dir = home + "/Applications"
            try? fm.createDirectory(atPath: dir, withIntermediateDirectories: true)
        }
        let dst = dir + "/Мост.app"
        if fm.fileExists(atPath: dst) { try? fm.removeItem(atPath: dst) }
        let cp = Process()
        cp.executableURL = URL(fileURLWithPath: "/usr/bin/ditto")
        cp.arguments = [src, dst]
        do { try cp.run(); cp.waitUntilExit() } catch { return false }
        guard cp.terminationStatus == 0 else { return false }
        // копию не помечаем «загружено из интернета» — иначе система снова спросит разрешение
        let xa = Process()
        xa.executableURL = URL(fileURLWithPath: "/usr/bin/xattr")
        xa.arguments = ["-dr", "com.apple.quarantine", dst]
        try? xa.run(); xa.waitUntilExit()
        quitting = true
        // отдельный экземпляр: иначе система лишь выведет вперёд этот (тот же bundle id), который сейчас закроется
        let oc = NSWorkspace.OpenConfiguration()
        oc.createsNewApplicationInstance = true
        oc.arguments = ["--moved"]
        NSWorkspace.shared.openApplication(at: URL(fileURLWithPath: dst), configuration: oc) { _, _ in
            DispatchQueue.main.async { NSApp.terminate(nil) }
        }
        return true
    }

    // ядро: дочерний процесс, первая строка вывода — адрес окна
    func startCore() {
        guard let exe = Bundle.main.url(forResource: "most-core", withExtension: nil) else { return fail("нет файла ядра") }
        let p = Process(); p.executableURL = exe
        let out = Pipe(); p.standardOutput = out
        coreOut = out
        // поток ошибок ядра — в файл: если ядро упадёт, причина останется (раньше терялась)
        let dir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support/Most")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let errURL = dir.appendingPathComponent("core-errors.log")
        if !FileManager.default.fileExists(atPath: errURL.path) { FileManager.default.createFile(atPath: errURL.path, contents: nil) }
        if let h = try? FileHandle(forWritingTo: errURL) { h.seekToEndOfFile(); p.standardError = h }
        // ядро завершилось само (не по «Выход») — перезапуск и новое окно
        p.terminationHandler = { [weak self] pr in
            DispatchQueue.main.async {
                guard let self = self, !self.quitting else { return }
                NSLog("most-core завершилось: код \(pr.terminationStatus), перезапуск")
                self.startCore()
                if let u = self.coreURL, let url = URL(string: u) { self.web?.load(URLRequest(url: url)) }
            }
        }
        do { try p.run() } catch { return fail("ядро не запустилось: \(error.localizedDescription)") }
        core = p
        var line = Data()
        while true {
            let b = out.fileHandleForReading.readData(ofLength: 1)
            if b.isEmpty || b == Data([10]) { break }
            line.append(b)
        }
        coreURL = String(data: line, encoding: .utf8)?.trimmingCharacters(in: .whitespaces)
    }

    func fail(_ msg: String) {
        let a = NSAlert(); a.messageText = "Мост"; a.informativeText = msg; a.runModal(); NSApp.terminate(nil)
    }

    func makeWindow() {
        let cfg = WKWebViewConfiguration()
        let uc = WKUserContentController()
        uc.add(self, name: "most")
        let installed = NSWorkspace.shared.urlForApplication(withBundleIdentifier: rdpBundleID) != nil
        // тот же мост для страницы, что и на Android (window.MostAndroid): копирование, выход, установка RDP-клиента
        let js = """
        window.MostAndroid = {
          copy: t => webkit.messageHandlers.most.postMessage({copy: t}),
          quit: () => webkit.messageHandlers.most.postMessage({quit: 1}),
          platform: () => "mac",
          rdpInstalled: () => \(installed ? "true" : "false"),
          installRdp: () => webkit.messageHandlers.most.postMessage({store: 1}),
          openRdpApp: () => webkit.messageHandlers.most.postMessage({openapp: 1})
        };
        """
        uc.addUserScript(WKUserScript(source: js, injectionTime: .atDocumentStart, forMainFrameOnly: true))
        cfg.userContentController = uc
        web = WKWebView(frame: .zero, configuration: cfg)
        web.uiDelegate = self
        web.navigationDelegate = self
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1040, height: 740),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable],
                          backing: .buffered, defer: false)
        window.title = "Мост"
        // обычная полоса заголовка (за неё окно перетаскивается; на всю высоту страница перехватывала мышь),
        // без надписи и в цвет фона страницы (--bg: светлая #f2f2f7, тёмная #000)
        window.titlebarAppearsTransparent = true
        window.titleVisibility = .hidden
        window.backgroundColor = NSColor(name: nil) { a in
            a.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua ? .black : NSColor(red: 0xf2/255, green: 0xf2/255, blue: 0xf7/255, alpha: 1)
        }
        window.minSize = NSSize(width: 420, height: 560)
        window.contentView = web
        window.delegate = self
        window.isReleasedWhenClosed = false
        window.center()
        if let u = coreURL, let url = URL(string: u) { web.load(URLRequest(url: url)) }
        if !hidden {
            window.makeKeyAndOrderFront(nil)
            NSApp.activate(ignoringOtherApps: true)
        }
    }

    func makeStatusItem() {
        status = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        if let img = NSImage(systemSymbolName: "point.3.connected.trianglepath.dotted", accessibilityDescription: "Мост") {
            img.isTemplate = true; status.button?.image = img
        } else { status.button?.title = "Мост" }
        let m = NSMenu()
        m.addItem(NSMenuItem(title: "Открыть «Мост»", action: #selector(show), keyEquivalent: ""))
        m.addItem(.separator())
        m.addItem(NSMenuItem(title: "Выход", action: #selector(quit), keyEquivalent: "q"))
        status.menu = m
    }

    @objc func show() { window.makeKeyAndOrderFront(nil); NSApp.activate(ignoringOtherApps: true) }
    @objc func quit() { quitting = true; core?.terminate(); NSApp.terminate(nil) }

    // закрытие окна — только спрятать: каналы и рабочий стол работают дальше
    func windowShouldClose(_ sender: NSWindow) -> Bool { sender.orderOut(nil); return false }
    // Windows App могли поставить, пока окно было открыто: при каждом возвращении в программу проверяем заново
    func applicationDidBecomeActive(_ n: Notification) {
        let ok = NSWorkspace.shared.urlForApplication(withBundleIdentifier: rdpBundleID) != nil
        web?.evaluateJavaScript("if(window.MostAndroid){MostAndroid.rdpInstalled=()=>\(ok);if(window.refresh)refresh()}")
    }
    func applicationShouldHandleReopen(_ s: NSApplication, hasVisibleWindows f: Bool) -> Bool { show(); return true }
    func applicationWillTerminate(_ n: Notification) { quitting = true; core?.terminate() }

    func userContentController(_ c: WKUserContentController, didReceive msg: WKScriptMessage) {
        guard let b = msg.body as? [String: Any] else { return }
        if let t = b["copy"] as? String {
            NSPasteboard.general.clearContents(); NSPasteboard.general.setString(t, forType: .string)
        } else if b["quit"] != nil {
            quit()
        } else if b["store"] != nil, let u = URL(string: rdpStoreURL) {
            NSWorkspace.shared.open(u)
        } else if b["openapp"] != nil {
            // открыть Windows App (настройка папки обмена); нет приложения — в App Store
            if let app = NSWorkspace.shared.urlForApplication(withBundleIdentifier: rdpBundleID) {
                NSWorkspace.shared.openApplication(at: app, configuration: NSWorkspace.OpenConfiguration())
            } else if let u = URL(string: rdpStoreURL) { NSWorkspace.shared.open(u) }
        }
    }

    // выбор файла профиля или приглашения
    func webView(_ w: WKWebView, runOpenPanelWith p: WKOpenPanelParameters, initiatedByFrame f: WKFrameInfo,
                 completionHandler: @escaping ([URL]?) -> Void) {
        let panel = NSOpenPanel(); panel.allowsMultipleSelection = false; panel.canChooseDirectories = false
        panel.message = "Файл профиля или приглашения"
        panel.begin { r in completionHandler(r == .OK ? panel.urls : nil) }
    }

    func webView(_ w: WKWebView, runJavaScriptConfirmPanelWithMessage m: String, initiatedByFrame f: WKFrameInfo,
                 completionHandler: @escaping (Bool) -> Void) {
        let a = NSAlert(); a.messageText = m; a.addButton(withTitle: "Да"); a.addButton(withTitle: "Отмена")
        completionHandler(a.runModal() == .alertFirstButtonReturn)
    }

    func webView(_ w: WKWebView, runJavaScriptAlertPanelWithMessage m: String, initiatedByFrame f: WKFrameInfo,
                 completionHandler: @escaping () -> Void) {
        let a = NSAlert(); a.messageText = m; a.runModal(); completionHandler()
    }

    // скачивание (профиль, приглашение — ссылки blob: с download) — сохранить файлом; внешние http(s) — в браузере
    func webView(_ w: WKWebView, decidePolicyFor a: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if a.shouldPerformDownload { decisionHandler(.download); return }
        if let u = a.request.url, let s = u.scheme, s == "blob" || s == "data" { decisionHandler(.download); return }
        if let u = a.request.url, let s = u.scheme, s == "http" || s == "https", u.host != "127.0.0.1",
           a.navigationType == .linkActivated {
            NSWorkspace.shared.open(u); decisionHandler(.cancel); return
        }
        decisionHandler(.allow)
    }

    func webView(_ w: WKWebView, decidePolicyFor r: WKNavigationResponse, decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
        decisionHandler(r.canShowMIMEType ? .allow : .download)
    }

    func webView(_ w: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) { download.delegate = self }
    func webView(_ w: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) { download.delegate = self }

    var saved: [ObjectIdentifier: URL] = [:]

    // куда сохранить: окно «Сохранить как», по умолчанию — «Загрузки»
    func download(_ d: WKDownload, decideDestinationUsing r: URLResponse, suggestedFilename: String,
                  completionHandler: @escaping (URL?) -> Void) {
        let p = NSSavePanel()
        p.nameFieldStringValue = suggestedFilename
        p.directoryURL = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first
        p.begin { res in
            guard res == .OK, let u = p.url else { return completionHandler(nil) }
            try? FileManager.default.removeItem(at: u) // замену уже подтвердили в окне; WKDownload поверх файла не пишет
            self.saved[ObjectIdentifier(d)] = u
            completionHandler(u)
        }
    }

    func downloadDidFinish(_ d: WKDownload) {
        if let u = saved.removeValue(forKey: ObjectIdentifier(d)) { NSWorkspace.shared.activateFileViewerSelecting([u]) }
    }

    func download(_ d: WKDownload, didFailWithError e: Error, resumeData: Data?) {
        saved.removeValue(forKey: ObjectIdentifier(d))
        let a = NSAlert(); a.messageText = "Файл не сохранился"; a.informativeText = e.localizedDescription; a.runModal()
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
