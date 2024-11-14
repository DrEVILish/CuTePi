const config = require('./config')
const sqlite3 = require('sqlite3').verbose();
const { open } = require('sqlite');

const initializeDB = async () => {
    return open({
        filename: config.db.location,
        driver: sqlite3.Database
    });
}

const dbPromise = initializeDB();

module.exports = dbPromise;
