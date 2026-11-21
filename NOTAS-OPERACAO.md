# Notas de operação

Anotado em 2026-11-21, depois da fila voltar a gravar lugares nesta máquina.
Isto descreve a instalação local (Docker + MySQL + SQLite) e o plano
ProxyScrape que ela usa. Não é o README do projeto upstream.

Não há chave, senha, usuário de proxy nem e-mail aqui. A chave fica só no
`.env` (`PROXYSCRAPE_API_KEY`).

## Esta instalação

Stack: `docker-compose.mysql.yaml`. Imagem `gmaps-local`, container
`google-maps-scraper-scraper-1`.

- UI: http://localhost:8081
- MySQL no host: `127.0.0.1:3307`, banco `gmaps`. A senha local está no
  próprio compose. Dentro da rede Docker o scraper fala com o host `mysql`
  na porta 3306.
- Fila e CSV: `./gmapsdata`, montado em `/gmapsdata`. Cada job grava
  `gmapsdata/<id>.csv` e também a tabela `places` do MySQL.
- Mailpit: http://127.0.0.1:8026. SMTP a partir do scraper: host `mailpit`,
  porta 1025, sem criptografia.
- O compose sobe um init (tini) como PID 1 e dá 2 GB de `/dev/shm`. Sem isso
  o Chromium deixa zumbi e aba estoura o shm padrão de 64 MB.
- `.dockerignore` exclui `gmapsdata`. A fila não entra na imagem.
- Build: `docker compose -f docker-compose.mysql.yaml up -d --build scraper`.
  O Bake avisa que o buildx não está instalado e cai no driver padrão. A
  camada do Playwright fica em cache; o que invalida `go mod download` é
  mudança em `third_party/`.
- Go do módulo: 1.27.4. Esta máquina não tem `go` nem `gofmt` no host. Teste
  e formatação rodam em `golang:1.27.4-trixie`, com o repositório montado em
  `/app`. Teste do pacote `jshttp` tem que rodar de dentro de
  `third_party/scrapemate`, não do módulo da raiz.
- `scrapemate` não é o módulo publicado: está em `third_party/scrapemate`,
  com `replace` no `go.mod`. O proxy autenticado mora em
  `third_party/scrapemate/adapters/fetchers/jshttp/local_proxy.go`.

O banco da fila é `gmapsdata/jobs.db` (SQLite, WAL). O arquivo fica
`root:root` e o usuário do host só lê. Leitura:

```bash
python3 -c 'import sqlite3; print(sqlite3.connect("file:gmapsdata/jobs.db?mode=ro", uri=True).execute("select status, count(*) from jobs group by status").fetchall())'
```

O container do scraper não tem `sqlite3`. Escrita no `jobs.db` a partir do
host precisa de um container rodando como root. Dois escritores ao mesmo
tempo funcionam por causa do WAL, com `busy_timeout`.

## A fila

Estados: `pending`, `working`, `ok`, `failed`.

A ordem de execução está em `web/sqlite/sqlite.go`: menos tentativas
primeiro, depois o mais antigo (`attempts ASC, created_at ASC`). Um job que
já falhou várias vezes fica atrás de um job novo.

`MaxJobAttempts` (3) é o número que a tela mostra. Não encerra o job.
`QueueRetry` devolve para `pending` todo job que ainda tem texto de busca.
Job sem keyword permanece `failed`, porque rodar de novo não tem o que
buscar.

Na subida, `RequeueInterrupted` tira de `working` o que o processo anterior
cortou e não conta essa queda como tentativa. Em seguida
`RequeueFailedRuns` devolve os `failed` que têm keyword. `SelectPendingN`
olha mais linhas do que o número de vagas, para um `RetryAfter` no futuro
não esconder um job que já pode rodar.

Fora o caso de 429 (abaixo), falha de proxy manda o job para o fim da fila:

- sem proxy utilizável: espera no mínimo 2 minutos (`proxyRetryDelay`);
- outras falhas: 1 minuto por tentativa, teto de 5 minutos (`maxRetryDelay`).

O texto de `ErrProxyCooling` ainda diz "goes back to the queue". O runner
não faz isso no 429. Ele espera e continua o mesmo job. Se algum caminho
ainda gravar essa string num job `failed`, `retryDelay` usa só a pausa
curta de preflight (20 s).

Workers: `jobWorkers` é metade das threads da CPU, salvo `WEB_JOB_WORKERS`
ou `-web-workers`. Nesta máquina o log sobe com até 4 jobs e 2 páginas de
browser cada. `perJobConcurrency` reparte as threads entre os jobs. Um `-c`
explícito ganha.

Modo normal desliga imagem (`DisableImages`, flag do Chromium
`imagesEnabled=false`). Modo fast usa stealth com Firefox. Inatividade do
scrapemate no web runner: 3 minutos. O ticker interno é de 1 minuto. Se
`lastActivityAt` ficar no ano 0001, o primeiro tick trata a run como já
ociosa e cancela. `Start()` precisa tocar esse relógio. Falha de seed chama
`IncrSeedCompleted` e libera a vaga sem esperar a inatividade.

String nova de template em português tem que existir em `web/i18n_ptbr.go`.
`TestPTBRCoversTemplateStrings` exige isso.

Cuidado ao ler log: a frase `no places found` contém `places found`.

## ProxyScrape, do jeito que esta conta se comporta

API da conta: `https://api.proxyscrape.com/v4/account`, header `api-token`.
Código em `web/proxyscrape.go`. Documentação pública:
https://docs.proxyscrape.com/api/account.

A chave enxerga duas subcontas `datacenter_shared`:

| Papel | AccountID | O que a chave consegue |
| --- | --- | --- |
| Trial | `649c62ee-5dc4-40e4-a7bb-cc072b04dcb1` | `GET .../proxy-list` responde 403, "Subaccount not authorized for this key" |
| Premium, a que tem cota | `20b06088-bc88-4622-b795-24956ad27dfb` | lista, overview e períodos |

`FetchProxies` percorre as duas. O 403 do trial é logado e ignorado. A
lista que vale é a do premium. Pedido: `format=credentials`,
`credential_format=3` (`http://user:pass@host:port`). Se a conta recusar
credencial, o código cai para `proxy_format=protocolipport`.

O que a conta premium mostrava por volta de 04:14 UTC de 2026-11-21:

- plano datacenter shared, assinatura ativa, não travada;
- banda do período: 250 GB (`250000000000` bytes);
- usado: cerca de 1,57 GB (`1570507496`); restante cerca de 248,4 GB;
- `bandwidth_use_multiplier` ligado, `bandwidth_usage_known` verdadeiro;
- 1000 proxies, país `us`, `max_connections` 5000, `maxips` 3;
- whitelist ligada e vazia, credencial de proxy ligada. O acesso é
  usuário/senha, não IP liberado;
- período corrente de `1795210917` a `1797802917`. O período tinha começado
  poucas horas antes. `total_periods_remaining` era 0;
- uso recente: um balde de cerca de 1,39 GB, outro de cerca de 63 MB, e o
  balde corrente em 0.

A cota mensal não estava esgotada. O 429 do gateway não era "acabou os
250 GB".

Lista ao vivo: 1000 URLs, um único usuário, 1000 hosts, porta `3129`,
esquema `http`. `maxSavedProxies` é 1000, então a lista cabe inteira.

O gateway, no CONNECT, responde assim quando recusa:

```text
HTTP/1.1 429
Retry-After: 120

Error 429: Not Enough Bandwidth. You have exceeded your bandwidth limit. Please try again later.
```

Isso é da conta inteira, não de uma saída. Outro IP da mesma lista recebe
o mesmo 429. Percorrer os 1000 hosts não desvia o limite: multiplica o
pedido que estourou. Um CONNECT só, recusado pelo gateway, já basta.

A documentação de limite de tráfego desse produto diz que não há teto de
Mbps, e que uso sustentado acima de cerca de 100 Mbps pode ser limitado.
Rajada curta eles tratam como normal. O multiplicador de preço em 1000
proxies (cerca de 1,48) é preço, não um fator que consome os 250 GB.

O `Retry-After: 120` é o que o gateway pede. Na credencial antiga ele não
descrevia a duração real do bloqueio: 12 minutos sem nenhum CONNECT
continuaram recebendo 429 com o mesmo header. Na credencial atual do
premium, uma busca no Maps passou e gravou lugares. O bloqueio teimoso
estava na lista velha, não num esgotamento da cota de 250 GB.

A API da conta e o gateway são coisas diferentes. Consultar overview,
períodos ou proxy-list não gasta a banda do proxy e não reinicia a janela
do 429. O Cloudflare na frente da API, porém, responde 1015 ("You are
being rate limited") se vários jobs baixam a lista ao mesmo tempo. Esse
1015 não é o 429 de banda. Com a lista já salva no job, a run segue com
ela.

## A lista fica congelada no job

O job guarda `data.proxies` no dia em que foi criado. O ProxyScrape troca
o usuário do plano. O usuário antigo continua autenticando e o gateway
responde 429 de banda. O usuário novo é o que tem os 248 GB.

Conferência feita sem imprimir usuário nem senha, só tamanho, host e um
hash curto:

- jobs presos: 100 proxies, 100 hosts, porta 3129, um usuário. Nenhum
  desses hosts estava na lista atual;
- lista atual: 1000 proxies, 1000 hosts, outro usuário;
- 210 jobs `ok` antigos ainda tinham a lista de 100. Eles tinham
  funcionado quando aquela credencial ainda passava;
- 32 jobs `ok` e alguns `pending` já tinham a lista de 1000.

Trocar a fila para a lista atual fez o preflight passar e o Maps devolver
lugares. Enquanto a fila discava a lista de 100, mais espera não resolvia.

No código, `scrapeJob` chama `AccountProxies` antes de discar quando a
chave está configurada e o processo não recebeu uma lista fixa por flag.
Essa chamada usa o cache de 10 minutos (`proxyScrapeCacheTTL`) e só fala
com a API da conta. A lista nova é gravada de volta no job. Se a API
falhar, fica a lista que o job já tem.

A imagem que rodou a recuperação de 2026-11-21 foi construída um passo
antes dessa troca: ela ainda chama `RefreshProxies`, que ignora o cache e
baixa a lista de novo a cada job. Foi isso que tomou o 1015 da Cloudflare.
O fallback era a lista já corrigida no SQLite, e a fila raspou assim mesmo.
O próximo build passa a usar o cache. Não reiniciei em cima dos jobs que
já estavam gravando.

`RefreshProxies` (força download) continua existindo para o caso em que a
lista do job se esgotou de verdade.

## O que o processo faz com o 429

Arquivos: `local_proxy.go`, `web/proxies.go`, `runner/webrunner/webrunner.go`.

1. Preflight testa no máximo 5 proxies desconhecidos, um por segundo
   (`proxyPreflightSample`, limiter 1/s). No primeiro 429, para. Os outros
   não são marcados como mortos.
2. Proxy que passou vale 10 minutos (`proxyGoodFor`). Proxy que falhou por
   outro motivo fica 15 minutos fora (`proxyBadFor`). 429 não entra nessa
   memória. Tratar 429 como proxy morto fez a fila inteira nascer com
   "tested 0 proxies".
3. O CONNECT que recebeu o 429 fecha na hora e o browser vê falha de túnel
   (502 local). Segurar o socket pelos 120 s estourava o timeout de 30 s
   do Playwright, e todo CONNECT estacionado acordava junto e estourava de
   novo.
4. Durante a pausa, `connectCandidate` recusa o dial localmente e não fala
   com o gateway. Um CONNECT de sonda no meio da pausa reinicia a janela
   do ProxyScrape. A API da conta pode ser consultada nessa janela. O
   gateway, não.
5. A pausa do processo é o `Retry-After`, com piso de 15 s se o header não
   vier, e teto de 3 minutos para um header absurdo. O caso desta conta é
   120 s, constante `AccountBandwidthPause`. Um segundo 429 no mesmo
   estouro não soma outro 120 s. Um 429 depois que a janela acabou é outros
   120 s, não 4 nem 6 minutos. A versão que multiplicava a pausa por
   "strikes" (2 min, depois 4, depois 6) foi retirada: ela alongava o
   bloqueio sem destravar a credencial.
6. `scrapeJob`, ao ver `ErrProxyCooling` ou uma passada de browser com zero
   lugares enquanto a pausa ainda está armada, loga
   `bandwidth limit: waiting …, then continuing`, dorme, e tenta o mesmo
   job de novo. O status continua `working`. Não chama `finishJob` nem
   `QueueRetry`, e não soma outra tentativa. O CSV é truncado na tentativa
   seguinte.
7. `docker stop` no meio dessa espera cancela o contexto. Aí sim o job é
   gravado como `failed`. Na subida seguinte `RequeueFailedRuns` devolve
   ele para a fila.
8. Depois do primeiro 429 do processo, `UpstreamLaneCap` deixa 1 job por
   vez por até 10 minutos. Duas buscas que acham lugares abrem para 2, e
   não voltam para os 4. Quatro browsers no instante em que a pausa acaba
   esvaziaram o balde de uma vez. Jobs que já estavam dentro da pausa
   quando o limite foi notado continuam in-flight: o primeiro a acordar
   disca, e os outros, se a pausa foi rearmada, esperam o resto sem discar.
9. Enquanto a pausa está armada, página nova do browser que ainda está
   aberto toma `net::ERR_TUNNEL_CONNECTION_FAILED`. Conexão que já estava
   de pé segue. Por isso uma busca pode achar a lista de lugares e perder
   alguma ficha no detalhe.
10. Busca que termina com zero lugares e sem pausa de banda vai para o fim
    da fila, com o erro `no places found`. Isso aconteceu uma vez na manhã
    em que a credencial nova já funcionava, com três browsers que tinham
    entrado juntos antes do limite de 1 vaga valer.

Antes de qualquer 429 neste processo, as 4 vagas abrem. O limite de 1 só
nasce depois do primeiro 429. Forçar 1 vaga desde a subida quebrava
`TestWorkRunsJobsInParallelUpToTheLimit`.

## O que esta manhã mostrou

Por volta de 01:57 UTC a fila parou. Dezenas de jobs com
`not started: no working proxies ... Too Many Requests`. No banco: 139
`failed` com 3 tentativas, 242 `ok`, nada em `pending` nem `working`. A
causa imediata era o preflight gravando o 429 como proxy morto por 15
minutos.

Corrigido isso, o 429 continuou, agora como pausa. A pausa de 120 s era
cortada, o socket do browser segurava o Retry-After, o Playwright caía em
30 s, e o ticker de inatividade via `lastActivityAt` em 0001-01-01 e
cancelava aos 60 s. Quatro jobs acordavam juntos no fim da janela e
tomavam outro 429. Sondas CONNECT feitas no meio da janela (três saídas,
todas 429 com Retry-After 120) alongaram o bloqueio. Doze minutos em
silêncio total, ainda na credencial antiga, não abriram o gateway.

Com a lista trocada para o premium atual, na imagem das 04:19 UTC:

- `7154eb62-66b1-490f-a51a-3a33a66dd736` achou 107 lugares (construtoras em
  Vitória/ES) e terminou `scraped successfully` às 04:24;
- `4a500d43-cd3a-4937-99e9-11f7d2216a0b` terminou `scraped successfully` às
  04:29;
- `53066441-382c-4797-b1a3-4689d6c0be5b` terminou `scraped successfully` às
  04:31;
- no caminho, outras buscas acharam 109, 106 e de novo 106 lugares;
- `61019316-aa83-4be0-ad7f-38485bbca3fa` voltou com 0 lugares e foi para o
  fim da fila;
- o contador de `ok` saiu de 242 para 245, e a fila seguiu com jobs novos
  em `working`.

Houve um 429 de banda também na credencial nova, no meio do primeiro
lote, e a pausa de 2 minutos segurou os outros jobs. As buscas que já
tinham a lista de lugares continuaram e gravaram. Às 04:29 o log passou a
`proxy bandwidth limit: running 2 at a time`, que é a abertura depois de
duas buscas com resultado.

## Três explicações comuns, medidas nesta conta

Comentário de diagnóstico posto ao lado do que a API e a fila mostraram
em 2026-11-21. As três causas circulam para o erro "Not Enough Bandwidth"
no Premium. Nesta conta só uma delas tem um pedaço que bate, e não é a
que segurou a fila a manhã inteira.

### 1. Fair use de plano "unlimited" (artigo 17.3)

Os termos, vigentes em 1 de setembro de 2026, dizem que plano de
datacenter vendido como "unlimited" inclui 1 TB para cada US$ 5 pagos,
com piso de 10 TB. O que passar disso pode ser cobrado, limitado ou
cortado. A página "What is the traffic limit?" separa dois produtos:
pacote unlimited, em que esse fair use é que vale, e pacote por banda,
com cota fixa (o exemplo deles é 250 GB). Esgotada a cota fixa, o proxy
para até recarga ou renovação.

Esta assinatura é o segundo tipo. O overview e o período devolvem
`bandwidth` de 250 GB, não um teto de 10 TB. Às 04:14 UTC o usado era
cerca de 1,57 GB e o restante cerca de 248,4 GB. O 429 apareceu aí. Não
houve cruzamento de 10 TB, nem da cota de 250 GB. O artigo 17.3 explica
um corte de plano unlimited no fim do ciclo. Não explica esta manhã.

### 2. Pool errado (residencial em vez de datacenter)

A mesma chave pode ter mais de uma subconta. Residencial é medido à
parte: com o saldo em 0 MB o gateway também fala em banda. Aqui as duas
subcontas são `datacenter_shared`. A trial responde 403 na proxy-list. A
premium responde a lista na porta 3129. `PROXYSCRAPE_RESIDENTIAL_GATEWAY`
não está definido, então o código nem monta URL residencial.

O que bate com "endpoint errado" é outro fato, dentro do datacenter: o
job guardava o usuário antigo, de 100 hosts, e a lista viva é outro
usuário, de 1000 hosts. Os dois autenticam. O antigo responde 429 de
banda. O novo raspa. Não era o gateway residencial, nem whitelist. A
whitelist está ligada e vazia, e a credencial de usuário/senha está
ligada. Um IP de servidor que mudou falharia a autenticação, não este
corpo de 429. A fila atual passa com usuário e senha.

### 3. Teto de Mbps

O mesmo documento de tráfego do Premium diz que não há teto de Mbps, e
que uso sustentado acima de cerca de 100 Mbps pode ser limitado para o
resto da rede. Rajada curta eles tratam como normal. O teto de 100 ou
200 Mbps citado para residential unlimited é outro produto.

Um CONNECT isolado, depois de 12 minutos sem ninguém discar, ainda tomou
429 na credencial antiga. Isso não é cano de Mbps cheio. Na credencial
nova, o 429 apareceu uma vez quando vários browsers saíram juntos, a
pausa de 120 s segurou o resto, e as buscas que já tinham lugares
gravaram. Imagem de página já nasce desligada fora do modo fast. Depois
do primeiro 429 o processo fica em 1 job e, com duas buscas que acham
lugar, abre para 2. É o ajuste certo para não sustentar vazão alta. Não
é o que destrava uma lista cujo usuário o gateway já recusa sozinho.

### O que fazer com isso

Enquanto a lista do job for a lista viva e o período mostrar centenas de
GB livres, a fila espera os 120 s e continua. Chat de suporte entra se a
lista viva, com o usuário certo, voltar a tomar 429 com o usado ainda
perto de 1,57 GB de 250 GB. Abaixo de 10 TB e abaixo da cota escrita na
conta, o fair use do artigo 17.3 não é o chamado.

## Como olhar sem piorar

- Contar status no `jobs.db` e ler `docker logs` do container do scraper.
  Sucesso é linha `scraped successfully`, ou `ok` acima do que estava, ou
  `N places found` com N maior que zero.
- Overview, períodos e proxy-list vão na API da conta.
- Comparar lista do job com a lista ao vivo por host, porta e um hash do
  usuário. O usuário em claro não precisa aparecer em log, nota nem diff.
- Um CONNECT de teste contra o gateway, feito para "ver se já passou",
  conta como uso e reinicia a janela dos 120 s. Durante a pausa o próprio
  processo já recusa o dial. Uma sonda externa fura isso.

## Onde mexer

| Assunto | Arquivo |
| --- | --- |
| Laço da fila, pausa, continuar o mesmo job | `runner/webrunner/webrunner.go` |
| Ordem da fila | `web/sqlite/sqlite.go` |
| Requeue, atraso entre tentativas | `web/retry.go` |
| Preflight, cache de saúde, 429 não é proxy morto | `web/proxies.go` |
| Download da lista e subcontas | `web/proxyscrape.go` |
| CONNECT, Retry-After, uma vaga depois do 429 | `third_party/scrapemate/adapters/fetchers/jshttp/local_proxy.go` |
| Relógio de inatividade | `third_party/scrapemate/scrapemate.go` (`Start` chama `stats.touch`) |
| Textos da UI em português | `web/i18n_ptbr.go` |
