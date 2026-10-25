# Vers & Belegd op BeepBite (proef)

Werknotitie voor Maikel Huijzer. Boslaantje Doen blijft de snackbar; CashDesk
blijft voorlopig de kassa daar. Vers & Belegd is de zakelijke lunch
(bedrijven, scholen, vergaderingen in Veenendaal en omgeving). Deze fork is
een **proefomgeving** om te zien of een bestelling van de bestelsite in de
keuken en op de kassa van BeepBite landt.

BeepBite is **pre-1.0**. Niet live zetten, geen cutover, geen productiegeheim
in deze repo.

De bestelsite is een aparte repo (`Maikelh83/vers-en-belegd-bestellen`,
Cloudflare Pages + D1 + Mollie, wizard en `/mijn/`). Die repo is in deze run
**niet gelezen**: GitHub antwoordde 404 op die naam, en er is niets aan
gewijzigd. Het JSON-contract hieronder is wat déze fork accepteert. De worker
op de bestelsite moet die vorm gaan POSTen; ga er niet van uit dat de nog
niet gemergede branch `cursor/b2b-lunch-wizard-424e` dat al doet.

## Proefomgeving starten

Postgres 16, Go 1.25 (de `go.mod` vraagt 1.25; een oudere Go haalt die
toolchain zelf op) en Node 20+. Docker is niet verplicht. `createdb beepbite`
of een lokale Postgres is genoeg. De webapp staat in `web/`, niet in de
repo-root, en Vite luistert op **poort 5174**.

```bash
# eenmalig
cp .env.example .env
# DATABASE_URL en JWT_SECRET (openssl rand -hex 32) invullen.
# CORS_ORIGINS moet http://localhost:5174 bevatten.

cd backend
go run ./cmd/migrate --env=local --up

# Nederlandse proeflocatie. Het menu blijft de demo-bistro
# "The Copper Table", niet de kaart van Boslaantje of Vers & Belegd.
SEED_COUNTRY=NL \
SEED_CURRENCY=EUR \
SEED_TIMEZONE=Europe/Amsterdam \
SEED_LOCALE=nl-NL \
SEED_TAX_RATE=9 \
SEED_TAX_INCLUSIVE=true \
SEED_TAX_LABEL=BTW \
SEED_PHONE_CC=31 \
  go run ./cmd/seedcopper --env=local --clean

go run ./cmd/server --env=local
```

In een tweede terminal:

```bash
cd web
npm install
npm run dev
# http://localhost:5174
```

`seedcopper` print de logins. Op deze fork, met het Copper Table-seed:

| Waar | Login |
|---|---|
| Dashboard (owner) | `demo@beepbite.app` / `Demo1234!` |
| Ook dashboard | `cashier@coppertable.test`, `chef@coppertable.test`, `manager@coppertable.test` — hetzelfde wachtwoord |
| Kassa-PIN (na het dashboard) | gebruikersnaam `aisha_pos`, PIN `5502` (meer PIN's print de seeder) |
| Locatie-slug | `the-copper-table--harbour-quarter` |

De upstream-README noemt ergens een kassier `cashier` met PIN `1234`. Dat is
niet wat `seedcopper` aanmaakt.

BTW in dit seed is **inclusief**, label `BTW`. Het locatie-default is 9%.
Eten, fris en koffie staan op een eigen tarief **BTW 9%**; wijn en cocktails
op **BTW 21%**. Order `BTW0001` heeft beide op één bon. Zie Gemengde BTW.

## Wat in Nederland klopt

BeepBite heeft geen land ingebakken. Per locatie stel je in:

| | Voor Boslaantje / Vers & Belegd |
|---|---|
| Valuta | `EUR` (2 decimalen). Bedragen in de database zijn centen. |
| Tijdzone | `Europe/Amsterdam`. Ordernummers, kassadag en rapportgrenzen volgen die dag, niet UTC. Een bestelling om 00:30 hoort bij die kalenderdag. |
| BTW-label | Vrij tekstveld. Zet `BTW`. |
| Inclusief | `tax_inclusive = true`. De kaartprijs is de prijs. De bon toont per tarief de grondslag en de BTW. |
| Telefoon | Landcode `31`. |

ESC/POS over het netwerk (TCP poort 9100) stuurt echt bytes, inclusief een
kassalade-kick. Op de bon staat `Prices in EUR` en bedragen zonder valutateken.
Dat is bewust: de meeste codepages hebben geen €, en de printer-code zet geen
codepage (PC858 of vergelijkbaar). Nederlandse tekens (ë, é, ï) gaan als UTF-8
de socket op en kunnen als rommel uit de printer komen. USB-printers zijn een
stub; er is geen POS-agent in deze repo. `window.print()` in de browser werkt
met elke printer die het besturingssysteem al kent.

Op rekening bestaat: huisrekeningen, leden, charges, facturen, en een
betaalwijze `house_account` op de kassa. Een lunch op rekening kan daarna
gefactureerd worden. De webhook hieronder kan een bestaande huisrekening
laden op **exacte naam** en er een charge op zetten. Hij maakt de rekening
niet zelf aan.

## Wat ontbreekt voor snackbar + catering

- **Geen gecertificeerd kassasysteem.** Er is een fiscale bonnummer-reeks,
  geen koppeling met een Nederlandse fiscus, geen controle- of
  afslagrapportage in de zin van de Belastingdienst, en geen traject voor het
  Keurmerk Betrouwbare Afrekensystemen. CashDesk niet uitzetten op basis van
  deze proef. De BTW-splitsing hieronder is een technische bon, geen aangifte
  en geen keurmerk.
- **ESC/POS en het euroteken / diakrieten**, hierboven.
- **Kassadag versus een paar rapportviews.** `business_date` is lokaal.
  Sommige oudere views groeperen nog met `AT TIME ZONE 'UTC'`. Rond middernacht
  kunnen die een uur scheef liggen ten opzichte van de kassalade.
- **Kolom `orders.fx_rate_to_zar`.** De applicatie leest hem niet. De default
  is 1. Geen stille omrekening naar rand, wel een naam die uit een eerdere
  fase is blijven staan.
- **Bezorgzones** bestaan (polygonen), maar deze koppeling controleert ze
  niet. Een adres buiten Veenendaal / Ede / Rhenen wordt gewoon aangenomen.
- **Bestelling voor later vandaag of morgen** gaat nú de keuken in. Status
  `confirmed` zet de order in `kds_fanout_queue`. Er is een `held_at`, maar de
  fan-out worker kijkt daar niet naar. Niet gebruiken als "vuur om 10:30".
- **Artikelen zonder stationsrouting** komen niet op het KDS, ook al bestaat
  de order. De worker verwijdert de wachtrijrij ook als er nul tickets uit
  komen.
- **Mollie zit niet in BeepBite** en hoort daar in deze proef ook niet. De
  bestelsite incasseert. BeepBite schrijft een betaalregel
  `external_prepaid` ("Paid on another channel") zodat de kassa de order niet
  nog een keer als openstaand toont. De kassier ziet die methode niet als knop
  in het afrekenvenster; die lijst is vast (`cash`, `card`, `gift_card`,
  `house_account`).
- **Contractprijzen.** De webhook vertrouwt de prijs van de site niet. De
  kaartprijs in BeepBite wint. Wijkt de site af, dan komt er een
  `price_warnings` in het antwoord en een regel in `orders.notes`. Een
  afgesproken bedrijfsprijs moet dus óf op de kaart staan, óf later als
  aparte prijslijst — die is er niet.
- **Klant zonder e-mail** wordt elke keer opnieuw aangemaakt. Met e-mail
  wordt de bestaande klant van die organisatie hergebruikt; naam en telefoon
  worden dan niet overschreven.
- **Telefoon** heeft geen eigen kolom op `customers` (alleen `whatsapp_number`).
  Het nummer staat in de notitie en op de keukenticket. Het wordt niet in het
  WhatsApp-veld gezet.
- **Sync tussen twee vestigingen** (snackbar en lunchkeuken) bestaat niet.
  Eén database, één proces.

## Gemengde BTW

Een artikel heeft een optioneel tarief (`items.tax_rate_id` → `tax_rates`).
Leeg betekent het locatie-default: de oudste actieve rij in `tax_rates`,
anders `locations.tax_rate`. Op de menukaart (Menu) kies je per artikel
minstens 9% en 21%, label BTW. De Nederlandse seed zet eten, fris en koffie
op **BTW 9%** en wijn plus cocktails op **BTW 21%**, en schrijft één proefbon
`BTW0001` (Americano + Espresso Martini).

Bij het aanslaan bewaart elke orderregel het tarief, of de prijs inclusief
was, de grondslag en de BTW. Prijzen blijven inclusief waar de locatie
`tax_inclusive` is: de klant betaalt de kaartprijs, de bon haalt de BTW eruit.
Meerdere regels met hetzelfde tarief worden één keer afgerond (niet per
regel, dat scheelt een cent). `orders.tax_cents` is de som. Delen de regels
niet één tarief, dan is `orders.tax_mixed` waar en `orders.tax_rate` is 0 —
dat is geen gemiddelde. De splitsing staat in `order_tax_lines` en, per
kassadag, in de view `order_tax_by_rate`.

De kassabon (scherm, PDF en ESC/POS) toont per tarief de grondslag en de
BTW, plus het totaal. Het keukenscherm blijft een bereidingsbriefje; de
bedragen staan op de regel, niet op het KDS. De webhook
`POST /webhooks/vers-en-belegd/orders` gebruikt het tarief van de SKU. Een
SKU zonder eigen tarief houdt het locatie-default. Het antwoord bevat
`tax_lines`. Een herhaald extern id blijft dezelfde order (HTTP 200,
`replayed: true`).

Bezorgkosten en fooi zitten niet in de BTW-grondslag. Chat-orders en tabs
rekenen nog met één tarief voor de hele order. De publieke menukaart schat
de BTW nog met het locatietarief; het bedrag dat de server schrijft, volgt
het artikel.

## Koppeling

De bestaande `channel.Channel` is een chatrail (WhatsApp: tekst, knoppen,
lijst). Een afgeronde checkout is dat niet. Die JSON in een chatbericht
proppen zou regels, bezorging, klant en idempotency kwijtraken.

Daarom staat er naast die chatrail een `channel.OrderSource`:

```go
type OrderSource interface {
    Name() string
    ParseOrder(body []byte) (InboundOrder, error)
}
```

`internal/channel/versenbelegd` is de eerste adapter. Hij kent alleen het
JSON-contract. `POST /webhooks/vers-en-belegd/orders` (geen staff-JWT; de
HMAC is het wachtwoord) schrijft de order.

Zet in `.env`, en hetzelfde geheim in de worker van de bestelsite. Niet in
git.

```bash
VERS_EN_BELEGD_WEBHOOK_SECRET=<openssl rand -hex 32>
VERS_EN_BELEGD_LOCATION_SLUG=the-copper-table--harbour-quarter
# optioneel, als de sku's niet gelijk zijn:
# VERS_EN_BELEGD_SKU_MAP=/pad/naar/sku-map.json
```

`sku-map.json` is een object `{"sku-op-de-site": "sku-in-beepbite"}`. Leeg
of afwezig betekent: de sku op de site ís `items.sku`. Zet die sku in
BeepBite op het artikel (uniek per locatie). Een onbekende of 86'de sku
weigert de **hele** order met 422. Er worden geen spookartikelen aangemaakt.

Header:

```text
X-Vers-En-Belegd-Signature: sha256=<hmac-sha256 van de rauwe body, hex>
```

Sleutel is het geheim hierboven. Een lege secret geeft 503, geen stille
acceptatie.

### Body

```json
{
  "id": "ord_lunch_1001",
  "placed_at": "2026-10-25T09:12:00+02:00",
  "fulfillment": {
    "type": "bezorg",
    "requested_for": "2026-10-25T11:30:00+02:00",
    "address": {
      "company": "Gemeente Veenendaal",
      "line1": "Raadhuisplein 1",
      "postal_code": "3901 GA",
      "city": "Veenendaal",
      "notes": "Receptie, 2e verdieping"
    }
  },
  "customer": {
    "name": "Jan de Vries",
    "email": "jan@example.nl",
    "phone": "+31612345678",
    "company": "Gemeente Veenendaal",
    "cost_center": "HR"
  },
  "lines": [
    {
      "sku": "broodje-gezond",
      "name": "Broodje gezond",
      "quantity": 8,
      "unit_price_cents": 650,
      "notes": "zonder ui"
    }
  ],
  "payment": {
    "mode": "betaald",
    "provider": "mollie",
    "reference": "tr_xxx",
    "house_account_name": ""
  },
  "notes": "Graag voor 11:30"
}
```

| Veld | Betekenis |
|---|---|
| `id` | Idempotency. Opgeslagen als `vers-en-belegd:<id>` (de kolom is uniek over de hele database). Dezelfde `id` nog eens POSTen geeft **200** en de bestaande order, geen tweede ticket. |
| `fulfillment.type` | `delivery` / `bezorg`, `collection` / `afhaal` / `pickup`, of `dine_in`. Bezorging zonder `address.line1` is 400. |
| `requested_for` | RFC3339. Afhalen → `pickup_at`. Bezorgen → `estimated_delivery_time`. Staat ook op de keukennotitie, in de tijdzone van de locatie. |
| `lines[].sku` | Na de optionele map: `items.sku` van deze locatie. |
| `lines[].unit_price_cents` | Wat de site toonde. De kaartprijs wint. |
| `payment.mode` | `paid` / `betaald` (Mollie-id verplicht in `reference`). `on_account` / `op_rekening` (`house_account_name` moet exact één actieve huisrekening zijn, zelfde valuta, onder het kredietlimiet). `pay_on_pickup` / `pay_on_delivery`: geen betaalregel, wél een keukenticket, notitie "nog niet betaald". |
| `payment.provider` | Alleen een label. `mollie` wordt niet aangeroepen. |

Antwoord 201 (of 200 bij herhaling):

```json
{
  "order_id": "…",
  "order_number": "VB0001",
  "status": "confirmed",
  "replayed": false,
  "total_cents": 5200,
  "currency": "EUR",
  "payment_mode": "paid",
  "price_warnings": [],
  "kitchen": "queued"
}
```

`kitchen: "queued"` betekent: de trigger heeft de order in de fan-out-wachtrij
gezet. Het ticket verschijnt pas als de worker draait (elke ~5 seconden, samen
met `cmd/server`) én het artikel naar een station routeert. Het is geen bewijs
dat het scherm het al toont.

Bezorgkosten: `locations.delivery_fee` (majeur, dus euro's) komt erbovenop,
niet in de BTW-grondslag van deze stub. `free_delivery_threshold` van 0 is
"geen regel", niet "altijd gratis". Boven een drempel > 0 vervalt de fee.
De zone en de kilometergrens worden niet gecheckt.

De kassadag (`business_date`) is de dag waarop BeepBite de webhook
**aanneemt**, in `Europe/Amsterdam`, niet de dag van `requested_for`.

Voorbeeld, vanaf de machine die het geheim heeft:

```bash
body=$(cat order.json)
sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$VERS_EN_BELEGD_WEBHOOK_SECRET" | awk '{print $2}')
curl -sS -D- -X POST http://localhost:8080/webhooks/vers-en-belegd/orders \
  -H "Content-Type: application/json" \
  -H "X-Vers-En-Belegd-Signature: sha256=$sig" \
  --data-binary "$body"
```

`openssl dgst -hmac` moet dezelfde bytes zien als de HTTP-body. Geen extra
newline.

## Volgende stappen

1. **Menu.** Vervang The Copper Table door de echte lunchkaart en de
   snackbar-artikelen die je in de proef wilt zien. Zet op elk artikel een
   sku die de bestelsite ook stuurt, en een keukenstation.
2. **Eén locatie of twee.** Snackbar en catering kunnen twee locaties in
   één database zijn. Deze koppeling schrijft naar precies één slug. Een
   tweede slug is een tweede geheim of een vervolgstap, niet deze stub.
3. **Huisrekeningen** handmatig aanmaken op de naam die de wizard stuurt
   (bedrijfsnaam), valuta EUR, kredietlimiet bewust gezet. Daarna
   `payment.mode = op_rekening`.
4. **Worker op de bestelsite** (niet in deze run): na een geslaagde Mollie-
   betaling of een bevestigde rekeningorder, POST het contract hierboven.
   Bewaar `order_id` / `order_number`. Retry met dezelfde `id`. Geen geheim
   in de frontend; alleen de worker.
5. **Hardware.** Netwerk-ESC/POS op poort 9100 in het LAN, of browserprint
   tot de codepage voor het Nederlands is uitgezocht. Geen USB verwachten.
6. **Hosting.** Eén machine die jij beheert: Postgres, de Go-binary, backups
   (`pg_dump`). Kassa en KDS mogen op het LAN. Alleen de webhook heeft
   publieke HTTPS nodig (een klein VPS-adres of een tunnel die jij draait).
   Er is geen BeepBite-cloud. De technische BTW-splitsing (9% en 21% op één
   bon) staat in deze fork. Een keurmerk of een fiscale certificaat is dat
   niet. Zet dit niet op de productie-kassa van Boslaantje tot die vraag
   apart is beantwoord.
7. **CashDesk** blijft de snackbarkassa tot je zelf anders besluit.

## Wat deze run heeft gedraaid

25 oktober 2026, op deze fork, tegen Postgres 16 (geen Docker op de machine;
`createdb` via een lokale cluster). De app-rol is `NOSUPERUSER` en
`NOBYPASSRLS`. Een superuser-verbinding zet row-level security stilletjes uit;
de server waarschuwt daarvoor. Niet zo draaien buiten een wegwerp-database.

`seedcopper --clean` met `SEED_COUNTRY=NL`, `EUR`, `Europe/Amsterdam`,
`nl-NL`, BTW 9% inclusief. De seeder printte `BTW 9.00%` en een voorbeeldprijs
`€ 125,00`. Het menu is nog steeds The Copper Table: een Americano staat daar
op €28,00. Dat is demodata, geen lunchkaart.

Twee gesigneerde POSTs tegen de draaiende server:

| Order | Pad | Resultaat |
|---|---|---|
| `ord_proef_1001` | bezorging, `betaald`, Mollie-referentie `tr_proef_1001`, sku `americano` × 2, siteprijs 650 cent | **201** `VB0001`, €91,00 (2 × €28,00 kaartprijs + €35,00 bezorging), BTW €4,62 in de prijs (`tax_inclusive`, tarief 9). `price_warnings` omdat 650 ≠ 2800. Zelfde body nog eens: **200**, `replayed: true`, dezelfde order. |
| `ord_proef_rekening_1` | afhaal, `op_rekening`, huisrekening Harbour Quarter Holdings, 4× americano | **201** `VB0002`, €112,00, betaalwijze `house_account`, charge van 11200 cent op die rekening. |

`VB0001` stond binnen één fan-out-cyclus op het KDS: station Bar, status fired,
2× Americano, notitie `proeforder Vers & Belegd`. De kassa op
`/pos/workspace` toont EUR en een regel `BTW`. Logins zoals in de tabel
hierboven (`demo@beepbite.app` / `Demo1234!`).

Daarna is gemengde BTW toegevoegd. Een nieuwe `seedcopper` (zonder `--clean`
is genoeg als het menu er al staat) schrijft `BTW0001`: Americano 9% en
Espresso Martini 21% op één bon, prijzen inclusief. De kassa en de bon tonen
beide grondslagen.
