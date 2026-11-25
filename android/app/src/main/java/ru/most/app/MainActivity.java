package ru.most.app;

import android.Manifest;
import android.app.Activity;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.view.Window;
import android.webkit.JavascriptInterface;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Toast;

/**
 * Окно «Моста»: страница ядра во встроенном WebView на весь экран — тот же дизайн, что на Windows.
 * Выбор файла профиля/приглашения — системным окном; внешние ссылки (rdp://, https://) — другими приложениями.
 */
public class MainActivity extends Activity {
    private WebView web;
    private ValueCallback<Uri[]> fileCb;
    private static final int PICK = 7;
    static final String RDP_PKG = "com.microsoft.rdc.androidx"; // Microsoft «Windows App» в Google Play

    @Override
    protected void onCreate(Bundle b) {
        super.onCreate(b);
        Window w = getWindow();
        w.setStatusBarColor(Color.TRANSPARENT);
        w.setNavigationBarColor(Color.TRANSPARENT);
        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 1);
        }
        startForegroundService(new Intent(this, MostService.class));

        web = new WebView(this);
        web.setBackgroundColor(Color.TRANSPARENT);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setAllowFileAccess(false);
        s.setAllowContentAccess(true);
        s.setTextZoom(100);
        web.addJavascriptInterface(new Bridge(), "MostAndroid");
        web.setWebViewClient(new WebViewClient() {
            @Override
            public boolean shouldOverrideUrlLoading(WebView v, WebResourceRequest r) {
                Uri u = r.getUrl();
                if ("127.0.0.1".equals(u.getHost())) return false; // своё окно
                try { startActivity(new Intent(Intent.ACTION_VIEW, u)); } catch (Exception ignored) {}
                return true;
            }
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onShowFileChooser(WebView v, ValueCallback<Uri[]> cb, FileChooserParams p) {
                if (fileCb != null) fileCb.onReceiveValue(null);
                fileCb = cb;
                Intent i = new Intent(Intent.ACTION_GET_CONTENT);
                i.addCategory(Intent.CATEGORY_OPENABLE);
                i.setType("*/*");
                startActivityForResult(Intent.createChooser(i, "Файл профиля или приглашения"), PICK);
                return true;
            }
        });
        // Android 15 рисует окно «от края до края» — отступы под строку состояния, вырез и жесты задаём сами.
        // WebView свои отступы игнорирует, поэтому они у контейнера.
        android.widget.FrameLayout box = new android.widget.FrameLayout(this);
        box.addView(web);
        box.setOnApplyWindowInsetsListener((v, ins) -> {
            if (Build.VERSION.SDK_INT >= 30) {
                android.graphics.Insets i = ins.getInsets(android.view.WindowInsets.Type.systemBars()
                        | android.view.WindowInsets.Type.displayCutout() | android.view.WindowInsets.Type.ime());
                v.setPadding(i.left, i.top, i.right, i.bottom);
            } else {
                v.setPadding(ins.getSystemWindowInsetLeft(), ins.getSystemWindowInsetTop(),
                        ins.getSystemWindowInsetRight(), ins.getSystemWindowInsetBottom());
            }
            return ins;
        });
        setContentView(box);
        waitAndLoad(0);
    }

    // ядро поднимается в службе за доли секунды — ждём адрес и открываем окно
    private void waitAndLoad(int tries) {
        if (MostService.url != null) {
            web.loadUrl(MostService.url);
        } else if (MostService.error != null) {
            web.loadData("<h3 style='font-family:sans-serif'>Ядро не запустилось: " + MostService.error + "</h3>", "text/html; charset=utf-8", "utf-8");
        } else if (tries < 100) {
            new Handler(Looper.getMainLooper()).postDelayed(() -> waitAndLoad(tries + 1), 100);
        }
    }

    @Override
    protected void onActivityResult(int req, int res, Intent data) {
        if (req == PICK && fileCb != null) {
            Uri[] r = (res == RESULT_OK && data != null && data.getData() != null) ? new Uri[]{data.getData()} : null;
            fileCb.onReceiveValue(r);
            fileCb = null;
        }
        super.onActivityResult(req, res, data);
    }

    @Override
    public void onBackPressed() {
        if (web.canGoBack()) web.goBack(); else moveTaskToBack(true); // назад не выключает «Мост»
    }

    /** Мост для страницы: копирование пароля для «Windows App», выключение. */
    class Bridge {
        @JavascriptInterface
        public void copy(String text) {
            ClipboardManager cm = getSystemService(ClipboardManager.class);
            cm.setPrimaryClip(ClipData.newPlainText("Мост", text));
            runOnUiThread(() -> Toast.makeText(MainActivity.this, "Скопировано", Toast.LENGTH_SHORT).show());
        }

        @JavascriptInterface
        public void quit() {
            startService(new Intent(MainActivity.this, MostService.class).setAction("stop"));
        }

        @JavascriptInterface
        public String platform() { return "android"; }

        /** Установлен ли Microsoft «Windows App» (клиент RDP). */
        @JavascriptInterface
        public boolean rdpInstalled() {
            try {
                getPackageManager().getPackageInfo(RDP_PKG, 0);
                return true;
            } catch (PackageManager.NameNotFoundException e) {
                return false;
            }
        }

        /** Открыть страницу «Windows App» в Google Play (или в браузере, если Google Play нет). */
        @JavascriptInterface
        public void installRdp() {
            runOnUiThread(() -> {
                try {
                    startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse("market://details?id=" + RDP_PKG)));
                } catch (Exception e) {
                    startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse("https://play.google.com/store/apps/details?id=" + RDP_PKG)));
                }
            });
        }
    }
}
