# Demo for the README. Record: vhs docs/demo.tape
# Needs Docker (Postgres from docker-compose.yml), Go, vhs. No API key: the model is the offline stub.
Output assets/demo.gif

Require go
Require docker

Set Shell "bash"
Set FontSize 14
Set Width 1300
Set Height 620
Set Padding 16
Set TypingSpeed 35ms

Hide
Type "make db >/dev/null 2>&1 && go build -o bin/lead-qualifier ./cmd/lead-qualifier && export PATH=$PWD/bin:$PATH DATABASE_URL=postgres://lq:lq@localhost:55432/lq?sslmode=disable && clear" Enter
Sleep 20s
Show

Type "lead-qualifier demo --offline 2>/dev/null" Enter
Sleep 2s
Type "Олег" Enter
Sleep 3s
Type "А сколько времени занимает подключение?" Enter
Sleep 3s
Type "У нас сеть из четырёх магазинов стройматериалов в Бердске, выручка около 12 млн в месяц. Я генеральный директор и владелец." Enter
Sleep 4s
Type "Давайте в 11" Enter
Sleep 5s
Ctrl+D
Sleep 3s
