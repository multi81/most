package ru.most.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.net.Uri;
import android.os.Build;
import android.os.IBinder;

/**
 * Фоновая служба «Моста»: держит ядро (каналы, автовыбор, локальный вход RDP) живым, пока открыт рабочий стол
 * в другом приложении. Android требует для этого постоянное уведомление.
 */
public class MostService extends Service {
    static volatile String url;      // адрес окна ядра (с ключом доступа)
    static volatile String error;    // ошибка запуска ядра

    private static final String CH = "most";

    @Override
    public void onCreate() {
        super.onCreate();
        NotificationManager nm = getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel(CH, "Мост работает", NotificationManager.IMPORTANCE_LOW));
        PendingIntent open = PendingIntent.getActivity(this, 0, new Intent(this, MainActivity.class),
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        Notification n = new Notification.Builder(this, CH)
                .setSmallIcon(R.drawable.ic_stat_most)
                .setContentTitle("Мост работает")
                .setContentText("Каналы до рабочего стола готовы")
                .setContentIntent(open)
                .setOngoing(true)
                .build();
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(1, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        } else {
            startForeground(1, n);
        }
        if (url == null) {
            try {
                url = mobile.Mobile.start(getFilesDir().getAbsolutePath(), link -> {
                    Intent i = new Intent(Intent.ACTION_VIEW, Uri.parse(link));
                    i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
                    startActivity(i); // бросит исключение, если нет приложения для rdp:// — ядро покажет подсказку
                });
            } catch (Exception e) {
                error = e.getMessage();
            }
        }
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (intent != null && "stop".equals(intent.getAction())) {
            stopForeground(STOP_FOREGROUND_REMOVE);
            stopSelf();
            android.os.Process.killProcess(android.os.Process.myPid()); // ядро Go остановить можно только вместе с процессом
        }
        return START_STICKY;
    }

    @Override
    public IBinder onBind(Intent intent) { return null; }
}
