// Package profile keeps the user's personal registry: identity, addresses, health
// (medications, supplements, diet, history), routine (enrollments, courses, habits),
// important dates and belongings. Sensitive items are encrypted at rest and only reach
// an AI model as allowed by PROFILE_AI_ACCESS.
package profile

// Field input types.
const (
	FText     = "text"
	FTextarea = "textarea"
	FDate     = "date"     // YYYY-MM-DD
	FTime     = "time"     // HH:MM
	FTimes    = "times"    // "08:00, 20:00"
	FWeekdays = "weekdays" // "seg,qua,sex" (empty = every day)
	FNumber   = "number"
	FSelect   = "select"
	FDay      = "day" // day of the month, 1-31
)

// Field is one attribute of a kind.
type Field struct {
	Key     string
	Label   string
	Type    string
	Help    string
	Options []string
	// Alert marks a date field that should show up in the upcoming list with this label.
	Alert string
}

// Kind describes one type of profile item.
type Kind struct {
	Key        string
	Label      string
	Icon       string
	Section    string
	TitleLabel string
	Sensitive  bool // default for new items; the user can change it per item
	Fields     []Field
}

// Section groups kinds on the profile page.
type Section struct {
	Key, Label, Icon, Help string
}

// Sections in display order ("overview" is the summary tab, not a kind group).
var Sections = []Section{
	{"perfil", "Sobre mim", "🪪", "Identidade, endereços, contatos de emergência, alergias e preferências."},
	{"saude", "Saúde", "🩺", "Medicações contínuas, suplementos, dieta, histórico, vacinas, exames e profissionais."},
	{"rotina", "Rotina", "🗓️", "Matrículas (academia, escola, idiomas), cursos, assinaturas e hábitos, com horários opcionais."},
	{"datas", "Datas", "🎂", "Aniversários, datas marcantes e viagens. Sugestões vindas do Google Agenda."},
	{"bens", "Casa e bens", "🏠", "Documentos, veículos, manutenção da casa, contas fixas, pets e garantias."},
	{"metas", "Metas", "🎯", "Objetivos com prazo e progresso."},
}

var weekdayField = Field{Key: "weekdays", Label: "Dias da semana", Type: FWeekdays, Help: "Nenhum marcado = todos os dias"}

// Kinds is the catalogue of item types.
var Kinds = []Kind{
	{Key: "identity", Label: "Identidade", Icon: "🪪", Section: "perfil", TitleLabel: "Nome completo", Sensitive: true, Fields: []Field{
		{Key: "nickname", Label: "Como prefere ser chamado", Type: FText},
		{Key: "birth", Label: "Nascimento", Type: FDate},
		{Key: "blood", Label: "Tipo sanguíneo", Type: FSelect, Options: []string{"", "A+", "A-", "B+", "B-", "AB+", "AB-", "O+", "O-"}},
		{Key: "documents", Label: "Documentos (RG, CPF…)", Type: FTextarea},
		{Key: "health_plan", Label: "Plano de saúde e carteirinha", Type: FText},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "address", Label: "Endereço", Icon: "📍", Section: "perfil", TitleLabel: "Nome (Casa, Trabalho…)", Sensitive: true, Fields: []Field{
		{Key: "address", Label: "Endereço", Type: FTextarea},
		{Key: "hours", Label: "Quando você está lá", Type: FText, Help: "Ex: seg a sex, 9h–18h"},
		{Key: "notes", Label: "Observações", Type: FTextarea, Help: "Portaria, vaga, senha do Wi-Fi…"},
	}},
	{Key: "emergency", Label: "Contato de emergência", Icon: "🆘", Section: "perfil", TitleLabel: "Nome", Sensitive: true, Fields: []Field{
		{Key: "relation", Label: "Relação", Type: FText},
		{Key: "phone", Label: "Telefone", Type: FText},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "allergy", Label: "Alergia ou restrição", Icon: "⚠️", Section: "perfil", TitleLabel: "Alergia/restrição", Sensitive: true, Fields: []Field{
		{Key: "severity", Label: "Gravidade", Type: FSelect, Options: []string{"", "leve", "moderada", "grave"}},
		{Key: "reaction", Label: "Reação e o que fazer", Type: FTextarea},
	}},
	{Key: "preference", Label: "Preferência", Icon: "⭐", Section: "perfil", TitleLabel: "Preferência", Fields: []Field{
		{Key: "detail", Label: "Detalhes", Type: FTextarea},
	}},

	{Key: "medication", Label: "Medicação contínua", Icon: "💊", Section: "saude", TitleLabel: "Medicamento", Sensitive: true, Fields: []Field{
		{Key: "dose", Label: "Dose", Type: FText, Help: "Ex: 50 mg, 1 comprimido"},
		{Key: "times", Label: "Horários", Type: FTimes, Help: "Ex: 08:00, 20:00. Recebe lembrete no Telegram"},
		weekdayField,
		{Key: "stock", Label: "Estoque (unidades)", Type: FNumber, Help: "Desconta a cada dose marcada; avisa quando estiver acabando"},
		{Key: "per_dose", Label: "Unidades por dose", Type: FNumber, Help: "Padrão 1"},
		{Key: "start", Label: "Início", Type: FDate},
		{Key: "end", Label: "Fim (se tiver)", Type: FDate, Alert: "Fim do tratamento"},
		{Key: "prescription", Label: "Receita válida até", Type: FDate, Alert: "Receita vence"},
		{Key: "doctor", Label: "Quem prescreveu", Type: FText},
		{Key: "notes", Label: "Observações", Type: FTextarea, Help: "Tomar em jejum, com comida…"},
	}},
	{Key: "supplement", Label: "Suplemento", Icon: "🧪", Section: "saude", TitleLabel: "Suplemento", Sensitive: true, Fields: []Field{
		{Key: "dose", Label: "Dose", Type: FText},
		{Key: "times", Label: "Horários", Type: FTimes},
		weekdayField,
		{Key: "stock", Label: "Estoque (doses)", Type: FNumber},
		{Key: "per_dose", Label: "Unidades por dose", Type: FNumber},
		{Key: "goal", Label: "Objetivo", Type: FText},
		{Key: "brand", Label: "Marca", Type: FText},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "diet", Label: "Dieta", Icon: "🥗", Section: "saude", TitleLabel: "Nome do plano", Sensitive: true, Fields: []Field{
		{Key: "goal", Label: "Objetivo", Type: FText},
		{Key: "kcal", Label: "Calorias/dia", Type: FNumber},
		{Key: "protein", Label: "Proteína (g)", Type: FNumber},
		{Key: "carbs", Label: "Carboidratos (g)", Type: FNumber},
		{Key: "fat", Label: "Gorduras (g)", Type: FNumber},
		{Key: "professional", Label: "Nutricionista", Type: FText},
		{Key: "start", Label: "Início", Type: FDate},
		{Key: "end", Label: "Fim ou reavaliação", Type: FDate, Alert: "Reavaliar dieta"},
		{Key: "plan", Label: "Plano alimentar", Type: FTextarea},
	}},
	{Key: "condition", Label: "Condição de saúde", Icon: "🩺", Section: "saude", TitleLabel: "Condição", Sensitive: true, Fields: []Field{
		{Key: "since", Label: "Desde", Type: FDate},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "surgery", Label: "Cirurgia ou internação", Icon: "🏥", Section: "saude", TitleLabel: "Procedimento", Sensitive: true, Fields: []Field{
		{Key: "date", Label: "Data", Type: FDate, Alert: "Cirurgia"},
		{Key: "place", Label: "Hospital", Type: FText},
		{Key: "doctor", Label: "Médico", Type: FText},
		{Key: "notes", Label: "Preparo, recuperação, observações", Type: FTextarea},
	}},
	{Key: "vaccine", Label: "Vacina", Icon: "💉", Section: "saude", TitleLabel: "Vacina", Sensitive: true, Fields: []Field{
		{Key: "date", Label: "Última dose", Type: FDate},
		{Key: "next", Label: "Próxima dose", Type: FDate, Alert: "Vacina"},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "exam", Label: "Exame", Icon: "🧾", Section: "saude", TitleLabel: "Exame", Sensitive: true, Fields: []Field{
		{Key: "date", Label: "Data", Type: FDate},
		{Key: "results", Label: "Resultados", Type: FTextarea},
		{Key: "next", Label: "Repetir em", Type: FDate, Alert: "Repetir exame"},
	}},
	{Key: "doctor", Label: "Profissional de saúde", Icon: "👩‍⚕️", Section: "saude", TitleLabel: "Nome", Sensitive: true, Fields: []Field{
		{Key: "specialty", Label: "Especialidade", Type: FText},
		{Key: "phone", Label: "Telefone", Type: FText},
		{Key: "place", Label: "Endereço", Type: FText},
		{Key: "last", Label: "Última consulta", Type: FDate},
		{Key: "every", Label: "Retorno a cada (meses)", Type: FNumber, Help: "Ex: 12 para check-up anual"},
		{Key: "next", Label: "Próxima consulta marcada", Type: FDate, Alert: "Consulta"},
	}},

	{Key: "enrollment", Label: "Matrícula", Icon: "🏋️", Section: "rotina", TitleLabel: "Onde (academia, escola…)", Fields: []Field{
		{Key: "activity", Label: "Atividade", Type: FText, Help: "Musculação, inglês, natação…"},
		weekdayField,
		{Key: "time", Label: "Horário de início", Type: FTime, Help: "Opcional. Lembrete antes da aula"},
		{Key: "until", Label: "Horário de fim", Type: FTime},
		{Key: "place", Label: "Endereço", Type: FText},
		{Key: "code", Label: "Nº da matrícula", Type: FText},
		{Key: "fee", Label: "Mensalidade (R$)", Type: FNumber},
		{Key: "due_day", Label: "Dia do pagamento", Type: FDay},
		{Key: "renew", Label: "Renovação ou fim do contrato", Type: FDate, Alert: "Renovar matrícula"},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "course", Label: "Curso", Icon: "🎓", Section: "rotina", TitleLabel: "Curso", Fields: []Field{
		{Key: "provider", Label: "Instituição/plataforma", Type: FText},
		weekdayField,
		{Key: "time", Label: "Horário", Type: FTime},
		{Key: "start", Label: "Início", Type: FDate, Alert: "Início do curso"},
		{Key: "end", Label: "Conclusão prevista", Type: FDate, Alert: "Fim do curso"},
		{Key: "progress", Label: "Progresso (%)", Type: FNumber},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "subscription", Label: "Assinatura", Icon: "🔁", Section: "rotina", TitleLabel: "Serviço", Fields: []Field{
		{Key: "price", Label: "Valor (R$)", Type: FNumber},
		{Key: "due_day", Label: "Dia da cobrança", Type: FDay},
		{Key: "renew", Label: "Renovação anual", Type: FDate, Alert: "Renovação"},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "habit", Label: "Hábito", Icon: "✅", Section: "rotina", TitleLabel: "Hábito", Fields: []Field{
		weekdayField,
		{Key: "times", Label: "Horários (opcional)", Type: FTimes, Help: "Com horário, recebe lembrete no Telegram"},
		{Key: "target", Label: "Meta", Type: FText, Help: "Ex: 2 litros de água"},
	}},

	{Key: "date", Label: "Data importante", Icon: "🎂", Section: "datas", TitleLabel: "O quê", Fields: []Field{
		{Key: "date", Label: "Data", Type: FDate},
		{Key: "category", Label: "Tipo", Type: FSelect, Options: []string{"aniversário", "casamento/namoro", "memória", "feriado pessoal", "outro"}},
		{Key: "yearly", Label: "Repete todo ano", Type: FSelect, Options: []string{"sim", "não"}},
		{Key: "person", Label: "Pessoa", Type: FText},
		{Key: "remind", Label: "Avisar com quantos dias", Type: FNumber, Help: "Padrão 7"},
		{Key: "notes", Label: "Ideias de presente, observações", Type: FTextarea},
	}},
	{Key: "trip", Label: "Viagem", Icon: "✈️", Section: "datas", TitleLabel: "Destino", Fields: []Field{
		{Key: "start", Label: "Ida", Type: FDate, Alert: "Viagem"},
		{Key: "end", Label: "Volta", Type: FDate},
		{Key: "bookings", Label: "Reservas (voo, hotel)", Type: FTextarea},
		{Key: "checklist", Label: "Documentos e checklist", Type: FTextarea},
	}},

	{Key: "document", Label: "Documento", Icon: "📄", Section: "bens", TitleLabel: "Documento (CNH, passaporte…)", Sensitive: true, Fields: []Field{
		{Key: "number", Label: "Número", Type: FText},
		{Key: "issued", Label: "Emissão", Type: FDate},
		{Key: "expires", Label: "Validade", Type: FDate, Alert: "Documento vence"},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "vehicle", Label: "Veículo", Icon: "🚗", Section: "bens", TitleLabel: "Veículo", Sensitive: true, Fields: []Field{
		{Key: "plate", Label: "Placa", Type: FText},
		{Key: "km", Label: "Km atual", Type: FNumber},
		{Key: "service", Label: "Próxima revisão", Type: FDate, Alert: "Revisão do veículo"},
		{Key: "insurance", Label: "Seguro até", Type: FDate, Alert: "Seguro vence"},
		{Key: "licensing", Label: "Licenciamento/IPVA", Type: FDate, Alert: "Licenciamento/IPVA"},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "home", Label: "Manutenção da casa", Icon: "🔧", Section: "bens", TitleLabel: "Item (filtro, ar-condicionado…)", Fields: []Field{
		{Key: "last", Label: "Última vez", Type: FDate},
		{Key: "every", Label: "Repetir a cada (meses)", Type: FNumber},
		{Key: "provider", Label: "Quem faz", Type: FText},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "bill", Label: "Conta fixa", Icon: "🧾", Section: "bens", TitleLabel: "Conta (aluguel, luz…)", Fields: []Field{
		{Key: "amount", Label: "Valor (R$)", Type: FNumber},
		{Key: "due_day", Label: "Dia do vencimento", Type: FDay},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},
	{Key: "pet", Label: "Pet", Icon: "🐾", Section: "bens", TitleLabel: "Nome", Fields: []Field{
		{Key: "species", Label: "Espécie/raça", Type: FText},
		{Key: "birth", Label: "Nascimento", Type: FDate},
		{Key: "vet", Label: "Veterinário", Type: FText},
		{Key: "vaccine", Label: "Próxima vacina", Type: FDate, Alert: "Vacina do pet"},
		{Key: "notes", Label: "Ração, remédios, observações", Type: FTextarea},
	}},
	{Key: "warranty", Label: "Garantia ou contrato", Icon: "🛡️", Section: "bens", TitleLabel: "Produto/contrato", Fields: []Field{
		{Key: "until", Label: "Válido até", Type: FDate, Alert: "Garantia/contrato vence"},
		{Key: "store", Label: "Loja/empresa", Type: FText},
		{Key: "notes", Label: "Observações", Type: FTextarea},
	}},

	{Key: "goal", Label: "Meta", Icon: "🎯", Section: "metas", TitleLabel: "Meta", Fields: []Field{
		{Key: "deadline", Label: "Prazo", Type: FDate, Alert: "Prazo da meta"},
		{Key: "progress", Label: "Progresso (%)", Type: FNumber},
		{Key: "why", Label: "Por que importa", Type: FTextarea},
		{Key: "next", Label: "Próximo passo", Type: FText},
	}},
}

var kindIndex = func() map[string]*Kind {
	m := make(map[string]*Kind, len(Kinds))
	for i := range Kinds {
		m[Kinds[i].Key] = &Kinds[i]
	}
	return m
}()

// KindOf returns the kind definition (nil when unknown).
func KindOf(key string) *Kind { return kindIndex[key] }

// SectionKinds returns the kinds of a section in catalogue order.
func SectionKinds(section string) []*Kind {
	var out []*Kind
	for i := range Kinds {
		if Kinds[i].Section == section {
			out = append(out, &Kinds[i])
		}
	}
	return out
}

// Weekdays are the keys used by FWeekdays fields, indexed by time.Weekday.
var Weekdays = []string{"dom", "seg", "ter", "qua", "qui", "sex", "sab"}
