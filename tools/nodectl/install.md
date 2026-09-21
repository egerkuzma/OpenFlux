# Панель управления нодой

Одна страница на самой ноде: состояние, параметры, журнал, заливка нового
бинарника. Появилась потому, что каждый опыт стоил одних и тех же четырёх
действий руками — собрать, залить, поставить от root, перезапустить — и потом
ещё поиска по журналу, чтобы понять, что вышло.

## Что нужно понимать до установки

Это **сетевая служба, которая запускает процессы**. Три решения приняты
сознательно:

- **Токен на каждом запросе.** Без него не отдаётся ничего, включая саму
  страницу.
- **Слушает только адрес ноды.** Публичного адреса у неё нет, значит панель
  доступна из локальной сети и через туннель — и больше ниоткуда.
- **Параметры не принимаются свободным текстом.** Флаги собираются из
  перечисленных в коде полей. Это не придирка: флаги ноды называют файлы, и
  непроверенный `--encryption-key-file` отдал бы наружу что угодно с машины.

Ссылки на документы проверяются по образцу и записываются в отдельный файл,
а не в командную строку.

## Установка

Всё ниже выполняется на ноде и требует root.

**1. Параметры ноды переезжают в отдельный файл.** Сейчас они вписаны прямо в
юнит, поэтому менять их можно только правкой юнита. Панели нужно место, куда
писать, не трогая юнит.

```
sudo install -d -m 755 /etc/openflux
sudo tee /etc/openflux/flags >/dev/null <<'EOF'
OPENFLUX_FLAGS=--role=exit --mode=l4 --transport=mailru --url-file=/etc/openflux/url.txt --encryption-key-file=/etc/openflux/secret.txt
EOF
sudo chown kuzmich /etc/openflux/flags /etc/openflux/url.txt
sudo chmod 644 /etc/openflux/flags /etc/openflux/url.txt
```

Файл с ключом остаётся недоступным: панель его не читает и не пишет, только
решает, передавать ли флаг.

**2. Юнит начинает брать параметры оттуда.**

```
sudo systemctl edit --full openflux
```

Заменить строку `ExecStart` и добавить `EnvironmentFile`:

```
EnvironmentFile=/etc/openflux/flags
ExecStart=/bin/sh -c 'exec /usr/local/bin/openflux $OPENFLUX_FLAGS'
```

Откат: вернуть прежнюю строку `ExecStart` и убрать `EnvironmentFile`.

**3. Права ровно на три команды.** Панель работает под обычным пользователем и
не может ни перезапустить службу, ни поставить бинарник. Даём только это:

```
sudo tee /etc/sudoers.d/nodectl >/dev/null <<'EOF'
kuzmich ALL=(root) NOPASSWD: /usr/bin/systemctl start openflux, /usr/bin/systemctl stop openflux, /usr/bin/systemctl restart openflux, /usr/bin/install -m755 -o root -g root /var/lib/nodectl/openflux.new /usr/local/bin/openflux
EOF
sudo chmod 440 /etc/sudoers.d/nodectl
sudo visudo -c
```

Путь к заливаемому файлу в правиле задан целиком — поставить что-то другое
этой записью нельзя. Откат: удалить файл.

**4. Сама панель.**

```
sudo install -d -m 755 -o kuzmich /var/lib/nodectl
sudo install -m 755 /tmp/nodectl /usr/local/bin/nodectl

sudo tee /etc/systemd/system/nodectl.service >/dev/null <<'EOF'
[Unit]
Description=Панель управления нодой OpenFlux
After=network-online.target

[Service]
User=kuzmich
Environment=NODECTL_ADDR=192.168.1.35:8787
Environment=NODECTL_TOKEN=ЗАМЕНИТЬ_НА_ДЛИННУЮ_СЛУЧАЙНУЮ_СТРОКУ
ExecStart=/usr/local/bin/nodectl
Restart=always
RestartSec=5
NoNewPrivileges=no

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now nodectl
```

Токен сгенерировать так: `openssl rand -hex 24`. Короче шестнадцати символов
служба не примет и не запустится.

**5. Открыть.**

```
http://192.168.1.35:8787/?t=<токен>
```

## Полный откат

```
sudo systemctl disable --now nodectl
sudo rm /etc/systemd/system/nodectl.service /etc/sudoers.d/nodectl /usr/local/bin/nodectl
sudo rm -rf /var/lib/nodectl
sudo systemctl edit --full openflux    # вернуть прежний ExecStart
sudo systemctl daemon-reload && sudo systemctl restart openflux
```
