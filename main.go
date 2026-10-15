package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	"strconv"

	"github.com/go-resty/resty/v2"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

// โครงสร้างข้อมูลตารางเรทเบี้ยประกัน (ใช้ min_age และ max_age)
type PremiumRate struct {
	ID         int         `json:"id"`
	PlanID     int         `json:"plan_id"`
	MinAge     int         `json:"min_age"`
	MaxAge     int         `json:"max_age"`
	AgeRange   string      `json:"age_range"`
	MaleRate   interface{} `json:"male_rate"`
	FemaleRate interface{} `json:"female_rate"`
}

// โครงสร้างข้อมูลแผนประกัน
type InsurancePlan struct {
	ID                int           `json:"id"`
	Title             string        `json:"title"`
	Category          string        `json:"category"`
	Description       string        `json:"description"`
	GoogleDriveFileID string        `json:"google_drive_file_id"`
	Slug              string        `json:"slug"`
	IsActive          bool          `json:"is_active"`
	Rates             []PremiumRate `json:"rates"`
}

// โครงสร้างข้อมูล Lead (ผู้สนใจสมัคร)
type LeadRequest struct {
	PlanID      int    `json:"plan_id"`
	FullName    string `json:"full_name"`
	PhoneNumber string `json:"phone_number"`
	LineID      string `json:"line_id"`
	DateOfBirth string `json:"date_of_birth"`
}

func main() {
	_ = godotenv.Load()

	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		connStr = "host=localhost port=5432 user=postgres password=1234 dbname=insuranceplan sslmode=disable"
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		log.Fatalf("ไม่สามารถเชื่อมต่อฐานข้อมูลได้: %v", err)
	}
	defer db.Close()

	if err = db.Ping(); err != nil {
		log.Fatalf("ฐานข้อมูลไม่ตอบสนอง: %v", err)
	}
	log.Println("เชื่อมต่อฐานข้อมูล PostgreSQL สำเร็จ!")

	app := fiber.New()

	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowHeaders: "Origin, Content-Type, Accept",
		AllowMethods: "GET,POST,PUT,DELETE,OPTIONS",
	}))

	// API ดึงข้อมูลแผนประกันทั้งหมด พร้อมเรทเบี้ย
	app.Get("/api/plans", func(c *fiber.Ctx) error {
		rows, err := db.Query("SELECT id, title, category, description, google_drive_file_id, slug, is_active FROM public.insurance_plans WHERE is_active = true")
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}
		defer rows.Close()

		var plans []InsurancePlan
		for rows.Next() {
			var p InsurancePlan
			if err := rows.Scan(&p.ID, &p.Title, &p.Category, &p.Description, &p.GoogleDriveFileID, &p.Slug, &p.IsActive); err != nil {
				continue
			}

			// ดึงเรทเบี้ยประกันโดยใช้ min_age และ max_age
			// ดึงเฉพาะคอลัมน์ที่มีอยู่จริง
			subRows, err := db.Query("SELECT id, plan_id, min_age, max_age, male_rate, female_rate FROM public.premium_rates WHERE plan_id = $1", p.ID)

			rates := []PremiumRate{}
			if err == nil {
				for subRows.Next() {
					var r PremiumRate
					// สแกนให้ตรงกับ 6 ฟิลด์ที่เรา Select มา
					if err := subRows.Scan(&r.ID, &r.PlanID, &r.MinAge, &r.MaxAge, &r.MaleRate, &r.FemaleRate); err != nil {
						continue
					}

					// เงื่อนไขจัดรูปแบบช่วงอายุ
					if r.MinAge == r.MaxAge {
						r.AgeRange = fmt.Sprintf("%d", r.MinAge)
					} else if r.MaxAge == 0 {
						r.AgeRange = fmt.Sprintf("%d", r.MinAge)
					} else {
						r.AgeRange = fmt.Sprintf("%d - %d", r.MinAge, r.MaxAge)
					}

					rates = append(rates, r)
				}
				subRows.Close()
			}
			p.Rates = rates

			plans = append(plans, p)
		}

		return c.JSON(plans)
	})

	// API บันทึก Leads และส่ง Telegram
	app.Post("/api/leads", func(c *fiber.Ctx) error {
		var req LeadRequest
		if err := c.BodyParser(&req); err != nil {
			return c.Status(400).JSON(fiber.Map{"error": "ข้อมูลไม่ถูกต้อง"})
		}

		var leadID int
		query := `INSERT INTO public.leads (plan_id, full_name, phone_number, line_id, date_of_birth, created_at) 
		          VALUES ($1, $2, $3, $4, $5, NOW()) RETURNING id`
		err := db.QueryRow(query, req.PlanID, req.FullName, req.PhoneNumber, req.LineID, req.DateOfBirth).Scan(&leadID)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "ไม่สามารถบันทึกข้อมูลลงฐานข้อมูลได้: " + err.Error()})
		}

		var planTitle string
		db.QueryRow("SELECT title FROM public.insurance_plans WHERE id = $1", req.PlanID).Scan(&planTitle)

		go sendTelegramNotification(planTitle, req)

		return c.JSON(fiber.Map{
			"success": true,
			"message": "บันทึกข้อมูลสำเร็จ",
			"lead_id": leadID,
		})
	})

	// API เพิ่มยอดวิวทีละ 1 เมื่อมีคนเปิดดูแผน

	app.Post("/api/plans/:id/view", func(c *fiber.Ctx) error {

		planID := c.Params("id")

		// ป้องกันการนับรัวๆ (สามารถเช็ค Session หรือทำ Cooldown เพิ่มเติมได้ที่นี่)

		_, err := db.Exec("UPDATE public.insurance_plans SET view_count = view_count + 1 WHERE id = $1", planID)

		if err != nil {

			return c.Status(500).JSON(fiber.Map{"error": "ไม่สามารถอัปเดตยอดวิวได้"})

		}

		return c.JSON(fiber.Map{"success": true})

	})

	// 1. API เพิ่มแผนประกันใหม่ (Create / Insert)
	app.Post("/api/plans", func(c *fiber.Ctx) error {

		var plan InsurancePlan
		if err := c.BodyParser(&plan); err != nil {
			println("PARSER ERROR DETECTED:", err.Error())
			return c.Status(400).JSON(fiber.Map{"error": "ข้อมูลแผนประกันไม่ถูกต้อง: " + err.Error()})
		}

		// 1. Insert ข้อมูลลงตาราง insurance_plans
		var planID int
		queryPlan := `INSERT INTO public.insurance_plans (title, category, description, google_drive_file_id, slug, is_active) 
					VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`

		err := db.QueryRow(queryPlan, plan.Title, plan.Category, plan.Description, plan.GoogleDriveFileID, plan.Slug, plan.IsActive).Scan(&planID)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "ไม่สามารถบันทึกแผนประกันได้: " + err.Error()})
		}

		// 2. วนลูป Insert เรทเบี้ยลงตาราง public.premium_rates (ถ้ามี)
		for _, rate := range plan.Rates {
			queryRate := `INSERT INTO public.premium_rates (plan_id, min_age, max_age, male_rate, female_rate) 
						VALUES ($1, $2, $3, $4, $5)`
			_, err = db.Exec(queryRate, planID, rate.MinAge, rate.MaxAge, rate.MaleRate, rate.FemaleRate)
			if err != nil {
				// ถ้าบันทึกเรทเบี้ยไม่ผ่าน สามารถข้ามหรือจัดการ Error ตามต้องการ
				continue
			}
		}

		return c.JSON(fiber.Map{
			"success": true,
			"message": "สร้างแผนประกันสำเร็จ",
			"plan_id": planID,
		})
	})

	// 2. API แก้ไขแผนประกัน (Update)
	app.Put("/api/plans/:id", func(c *fiber.Ctx) error {

		// 1. ลอง Print ดู JSON ดิบๆ ที่ส่งมาจากหน้าบ้าน

		bodyBytes := c.Body()

		println("RAW BODY RECEIVED:", string(bodyBytes))

		planID := c.Params("id")
		var plan InsurancePlan
		if err := c.BodyParser(&plan); err != nil {
			println("PARSER ERROR DETECTED:", err.Error())
			return c.Status(400).JSON(fiber.Map{"error": "ข้อมูลที่ส่งมาไม่ถูกต้อง: " + err.Error()})
		}

		// 1. Update ข้อมูลหลักในตาราง insurance_plans
		queryUpdate := `UPDATE public.insurance_plans 
						SET title = $1, category = $2, description = $3, google_drive_file_id = $4, slug = $5, is_active = $6 
						WHERE id = $7`
		_, err := db.Exec(queryUpdate, plan.Title, plan.Category, plan.Description, plan.GoogleDriveFileID, plan.Slug, plan.IsActive, planID)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "ไม่สามารถอัปเดตแผนประกันได้: " + err.Error()})
		}

		// 2. ลบเรทเบี้ยชุดเก่าทิ้งทั้งหมดของแผนนี้
		_, err = db.Exec("DELETE FROM public.premium_rates WHERE plan_id = $1", planID)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "ไม่สามารถเคลียร์เรทเบี้ยเก่าได้: " + err.Error()})
		}

		// 3. Insert เรทเบี้ยชุดใหม่เข้าไปแทน
		for _, rate := range plan.Rates {
			maleRate := parseToFloat(rate.MaleRate)
			femaleRate := parseToFloat(rate.FemaleRate)

			queryRate := `INSERT INTO public.premium_rates (plan_id, min_age, max_age, male_rate, female_rate, created_at) 
						VALUES ($1, $2, $3, $4, $5, NOW())`
			_, err = db.Exec(queryRate, planID, rate.MinAge, rate.MaxAge, maleRate, femaleRate)
			if err != nil {
				continue
			}
		}

		return c.JSON(fiber.Map{
			"success": true,
			"message": "อัปเดตแผนประกันสำเร็จ",
		})
	})

	// 3. API ลบแผนประกัน (Delete)
	app.Delete("/api/plans/:id", func(c *fiber.Ctx) error {
		planID := c.Params("id")

		// 1. ลบเรทเบี้ยที่เกี่ยวข้องออกก่อน (ป้องกันติด Foreign Key Constraint)
		_, err := db.Exec("DELETE FROM public.premium_rates WHERE plan_id = $1", planID)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "ไม่สามารถลบเรทเบี้ยได้: " + err.Error()})
		}

		// 2. ลบตัวแผนประกันหลัก
		_, err = db.Exec("DELETE FROM public.insurance_plans WHERE id = $1", planID)
		if err != nil {
			return c.Status(500).JSON(fiber.Map{"error": "ไม่สามารถลบแผนประกันได้: " + err.Error()})
		}

		return c.JSON(fiber.Map{
			"success": true,
			"message": "ลบแผนประกันสำเร็จ",
		})
	})

	log.Println("Go Backend กำลังรันที่พอร์ต 8080...")
	log.Fatal(app.Listen(":8080"))
}

func sendTelegramNotification(planTitle string, req LeadRequest) {
	botToken := os.Getenv("8734324884:AAHT5dVsZ9GWfUyxBrJy6EO0DIaglJxVQMY")
	chatID := os.Getenv("6922016520")

	if botToken == "" || chatID == "" {
		return
	}

	message := fmt.Sprintf(
		"🔔 *มีลูกค้าสนใจสมัครประกันใหม่!*\n\n"+
			"📋 *แผนประกัน:* %s\n"+
			"👤 *ชื่อ-นามสกุล:* %s\n"+
			"📞 *เบอร์โทรศัพท์:* %s\n"+
			"💬 *Line ID:* %s\n"+
			"🎂 *วันเกิด:* %s",
		planTitle, req.FullName, req.PhoneNumber, req.LineID, req.DateOfBirth,
	)

	client := resty.New()
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)

	_, _ = client.R().
		SetBody(map[string]interface{}{
			"chat_id":    chatID,
			"text":       message,
			"parse_mode": "Markdown",
		}).
		Post(url)
}

// ฟังก์ชันแปลงค่า interface{} เป็น int ปลอดภัย

func toInt(val interface{}) int {

	if val == nil {

		return 0

	}

	switch v := val.(type) {

	case float64:

		return int(v)

	case int:

		return v

	case string:

		parsed, _ := strconv.Atoi(v)

		return parsed

	}

	return 0

}

// ฟังก์ชันแปลง interface{} เป็น float64 ปลอดภัย

func parseToFloat(val interface{}) float64 {

	if val == nil {

		return 0.0

	}

	switch v := val.(type) {

	case float64:

		return v

	case int:

		return float64(v)

	case string:

		if v == "" {

			return 0.0

		}

		parsed, err := strconv.ParseFloat(v, 64)

		if err != nil {

			return 0.0

		}

		return parsed

	}

	return 0.0

}
